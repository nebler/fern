# fold.pl MODE START END: reads `perf script` output (fields comm, tid,
# time, [trace,] ip, sym, dso) and prints folded stacks,
# "comm;outer;...;leaf value", for flamegraph.pl, of what happened between
# START and END (seconds):
#   on:  CPU samples, one per sample.
#   off: time off the CPU, in microseconds, from sched_switch events: a
#        thread's stack when it left the CPU, for the part of the wait until
#        it came back that lies in the window. Waits that don't end in the
#        recording (threads idle throughout) aren't counted.
use strict;
use warnings;

my ($mode, $start, $end) = @ARGV;
die "usage: fold.pl on|off START END\n" unless defined $end && $mode =~ /^(on|off)$/;

my %folded;
my %pending;    # tid => [time it left, folded stack]
my ($comm, $tid, $time, $trace, @frames);

# frame names a frame. One in a binary without symbols (stripped Go:
# k3s, runsc) is "?PATH@0xADDRESS", for symbolize.py; code with no file
# (a JIT, or a box's program under gVisor) is "[anonymous code]".
sub frame {
  my ($ip, $sym, $dso) = @_;
  # A stripped Go binary can keep a few data symbols, which perf then gives
  # every frame after them: those are unknown too.
  $sym = '[unknown]' if $sym =~ /^(runtime\.(pclntab|epclntab|etext|text|findfunctab|noptrdata|rodata|types|etypes)|go:buildid)$/;
  if ($sym eq '[unknown]') {
    return '[anonymous code]' if $dso =~ m{/perf-\d+\.map$} || $dso eq '[unknown]';
    return "?$dso\@0x$ip" if $dso =~ m{^/};
    $dso =~ s{.*/}{};
    return "[$dso]";
  }
  $sym =~ s/;/:/g;
  return $sym;
}

sub flush {
  return unless defined $time;
  my $stack = join ';', $comm, reverse @frames;
  if ($mode eq 'on') {
    $folded{$stack}++ if $time >= $start && $time <= $end;
  } elsif (defined $trace && ($trace =~ /prev_comm=(.*?) prev_pid=(\d+).*==> next_comm=.*? next_pid=(\d+)/
      || $trace =~ /^(.*):(\d+) \[-?\d+\] \S+ ==> .*:(\d+) \[-?\d+\]$/)) {
    # The old format (prev_comm=…) or perf's newer "comm:pid [prio] state ==> comm:pid [prio]".
    my ($prev_comm, $prev, $next) = ($1, $2, $3);
    $prev_comm =~ s/[ ;]/_/g;
    if (my $p = delete $pending{$next}) {
      my $from = $p->[0] > $start ? $p->[0] : $start;
      my $to = $time < $end ? $time : $end;
      $folded{$p->[1]} += ($to - $from) * 1e6 if $to > $from;
    }
    if ($prev != 0) {    # 0 is the idle task
      $pending{$prev} = [$time, join ';', $prev_comm, reverse @frames];
    }
  }
  undef $time;
  @frames = ();
}

while (my $line = <STDIN>) {
  chomp $line;
  if ($line =~ /^\s*(.+?)\s+(\d+)\s+(\d+\.\d+):\s*(.*)$/) {
    flush();
    ($comm, $tid, $time, $trace) = ($1, $2, $3, $4);
    $comm =~ s/[ ;]/_/g;
  } elsif ($line =~ /^\s+([0-9a-f]+)\s+(.*?)\s+\((.*)\)$/) {
    push @frames, frame($1, $2, $3);
  } elsif ($line eq '') {
    flush();
  }
}
flush();
for my $stack (sort keys %folded) {
  printf "%s %d\n", $stack, $folded{$stack} if $folded{$stack} >= 1;
}

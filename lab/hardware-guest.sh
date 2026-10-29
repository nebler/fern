#!/bin/sh
# One guest /proc sample. gVisor approximates RSS; summing process RSS can
# double-count shared pages. Host pod anon+shmem remains the capacity metric.
awk '
  FILENAME ~ /\/stat$/ {
    pid=FILENAME; sub("/proc/","",pid); sub("/stat","",pid)
    line=$0; sub(/^.*\) /,"",line); split(line,s," ")
    parent[pid]=s[2]; cpu[pid]=(s[12]+s[13])*10; birth[pid]=s[20]
    name=$0; sub(/^[^(]*\(/,"",name); sub(/\).*$/,"",name)
    if(name ~ /^claude/) root[pid]=1
  }
  FILENAME ~ /\/status$/ {
    pid=FILENAME; sub("/proc/","",pid); sub("/status","",pid)
    if($1=="VmRSS:") rss[pid]=$2
  }
  FILENAME ~ /\/smaps$/ {
    pid=FILENAME; sub("/proc/","",pid); sub("/smaps","",pid)
    if($1=="Anonymous:") anon[pid]+=$2
  }
  END {
    for(pid in parent) {
      p=pid; found=0
      for(i=0;i<100 && p in parent;i++) {if(p in root) {found=1;break} p=parent[p]}
      printf "GUEST %s_%s %.3f %.3f %.3f %s\n",pid,birth[pid],cpu[pid],rss[pid]/1024,anon[pid]/1024,(found?"claude":"other")
    }
  }' /proc/[0-9]*/stat /proc/[0-9]*/status /proc/[0-9]*/smaps 2>/dev/null

"""symbolize.py: names the frames perf couldn't, in folded stacks.

k3s (kubelet, containerd), runsc and the controllers are stripped Go
binaries: perf finds no symbols in them, so fold.pl leaves their frames as
"?PATH@ADDRESS". Go keeps its own table of functions (pclntab) in a stripped
binary, which "go tool addr2line" reads.

  symbolize.py list FOLDED...        prints "PATH ADDRESS" per unknown frame
  symbolize.py apply NAMES FOLDED... rewrites the files, NAMES holding lines
                                     "PATH ADDRESS FUNCTION"

A frame with no name, or of a binary that couldn't be read, becomes
"[basename]". Stacks that come out the same are added up.
"""

import os
import re
import sys

FRAME = re.compile(r"\?(/[^@;]+)@(0x[0-9a-f]+)")


def frames(paths):
    seen = set()
    for path in paths:
        with open(path) as f:
            for line in f:
                for m in FRAME.finditer(line):
                    seen.add((m.group(1), m.group(2)))
    return sorted(seen)


def apply(names_file, paths):
    names = {}
    with open(names_file) as f:
        for line in f:
            parts = line.rstrip("\n").split(" ", 2)
            if len(parts) == 3 and parts[2] not in ("?", ""):
                names[(parts[0], parts[1])] = parts[2].replace(";", ":")

    def name(m):
        return names.get((m.group(1), m.group(2)), "[" + os.path.basename(m.group(1)) + "]")

    for path in paths:
        stacks = {}
        with open(path) as f:
            for line in f:
                stack, _, value = line.rstrip("\n").rpartition(" ")
                stack = FRAME.sub(name, stack)
                stacks[stack] = stacks.get(stack, 0) + int(value)
        with open(path, "w") as f:
            for stack in sorted(stacks):
                f.write(f"{stack} {stacks[stack]}\n")


if __name__ == "__main__":
    if sys.argv[1] == "list":
        for p, a in frames(sys.argv[2:]):
            print(p, a)
    elif sys.argv[1] == "apply":
        apply(sys.argv[2], sys.argv[3:])
    else:
        sys.exit(__doc__)

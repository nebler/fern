#!/bin/sh
# Run in a box, in its worktree, by lab/check.sh: what a box that means harm
# does to get its program run by a git outside it. "make" makes a repository
# in the folder nested/, as git init does. "arm", once the worktree's index
# holds that folder as a submodule, gives the folder's own config a filter
# and an ssh command that leave a mark at $2, changes a file there so that
# git has a reason to look, and writes a .gitmodules that asks git to look
# into the folder whatever its settings say.
set -eu
case $1 in
make)
	git init -q nested
	cd nested
	echo f >f
	git add f
	git -c user.name=box -c user.email=box@fern commit -qm theirs
	;;
arm)
	mark=$2
	cd nested
	git config filter.mark.clean "sh -c 'echo ran >> $mark; cat'"
	git config core.sshCommand "sh -c 'echo ran >> $mark'"
	git config remote.origin.url ssh://nowhere.invalid/x
	echo '* filter=mark' >.git/info/attributes
	echo changed >f
	cd ..
	printf '[submodule "nested"]\n\tpath = nested\n\turl = ssh://nowhere.invalid/x\n\tignore = none\n\tfetchRecurseSubmodules = true\n' >.gitmodules
	;;
esac

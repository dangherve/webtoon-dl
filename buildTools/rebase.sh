#!/bin/bash
set -e
git remote add upstream https://github.com/robinovitch61/webtoon-dl.git
git fetch upstream
git rebase upstream/main
git remote set-url origin git@github.com:dangherve/webtoon-dl.git
git push --force --set-upstream origin master

#/bin/bash


while read url; do
    ./webtoon-dl  -min-ep 1 -max-ep 1 $url&
done < liste

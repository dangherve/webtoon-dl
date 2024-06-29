#/bin/bash

while read url; do
    ./webtoon-dl  -min-ep 1 -max-ep 1 -file -W 1 -E 1 $url
    if [ $? -ne 0 ]; then
        echo $url
    fi
done < liste

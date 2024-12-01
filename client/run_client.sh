#!/bin/bash

count=0
while :; do
    if ! netstat --inet -n -a -p 2> /dev/null | grep ":$(($count+9050))" ; then
        break
    fi
    count=$(($count+1))
done

cp torrc-client torrc-$count
sed -i -e "s/datadir/datadir$count/g" torrc-$count
sed -i -e "/^-url http:\/\/localhost:8080\//a -log snowflake_client-$count.log" torrc-$count
echo "SOCKSPort $(($count+9050))" >> torrc-$count

tor -f torrc-$count > client-$count.log 2> client-$count.err &
#!/bin/bash

master="llb1"
backup="llb2"

function check_ha() {
    # The retry budget is counted across iterations, so it is set once,
    # before the loop: reset inside it, the limit below could never be met.
    count=0
    while : ; do
        status1=$($hexec llb1 curl -sX 'GET' 'http://0.0.0.0:11111/netlox/v1/config/cistate/all' -H 'accept: application/json' | jq -r '.Attr[0].state')
        status2=$($hexec llb2 curl -sX 'GET' 'http://0.0.0.0:11111/netlox/v1/config/cistate/all' -H 'accept: application/json' | jq -r '.Attr[0].state')
        if [[ $status1 == "MASTER" && $status2 == "BACKUP" ]];
        then
            master="llb1"
            backup="llb2"
            break
        elif [[ $status2 == "MASTER" && $status1 == "BACKUP" ]]; 
        then
            master="llb2"
            backup="llb1"
            break
        else
            count=$(( $count + 1 ))
            if [[ $count -ge 20 ]]; then
                echo "KA llb1-$status1, llb2-$status2 [NOK] - Exiting" >&2
                exit 1;
            fi
            echo "KA llb1-$status1, llb2-$status2 [NOK]" >&2
            sleep 5
        fi
    done
}

# Prints the case's live association on a node as one line per entry: the
# established entries of service $2 whose client port is $3, keyed by
# service, addresses, ports and protocol, with the packet count last.
function liveAssoc() {
    $dexec $1 loxicmd get ct --servName=$2 | tr -s ' ' | awk -F'|' -v port=" $3 " '
        $9 == " est " && ($5 == port || $6 == port) {print $2 "|" $3 "|" $4 "|" $5 "|" $6 "|" $7 "|" $11}'
}

# Checks the association of service $1 with client port $2 while its client
# is still sending. Returns 0 when the MASTER holds it established on every
# path, its packet counts are still rising, and the BACKUP holds the same
# entries; 1 when the BACKUP still differs after the retries; 2 when the
# MASTER holds none of it. Entries are compared by identity, never by count:
# a closed association can linger as established on a BACKUP, which ages it
# out without a sync. The result is the return code only: callers read it
# with `checkLiveSync svc port; rc=$?`, never from its output.
function checkLiveSync() {
    local svc=$1 port=$2 count=1 m1 m2 b k1 k2 kb p1 p2
    while [[ $count -le 5 ]] ; do
        m1=$(liveAssoc $master $svc $port)
        sleep 1
        m2=$(liveAssoc $master $svc $port)
        b=$(liveAssoc $backup $svc $port)
        echo -e "\nLive association at MASTER:$master\n$m2" >&2
        echo -e "\nLive association at BACKUP:$backup\n$b" >&2

        if [[ -z $m2 ]]; then
            echo -e "No live association of $svc in Master:$master. Exiting!" >&2
            return 2
        fi
        k1=$(echo "$m1" | cut -d'|' -f1-6 | sort)
        k2=$(echo "$m2" | cut -d'|' -f1-6 | sort)
        kb=$(echo "$b" | cut -d'|' -f1-6 | sort)
        p1=$(echo "$m1" | cut -d'|' -f7 | awk '{s+=$1} END{print s+0}')
        p2=$(echo "$m2" | cut -d'|' -f7 | awk '{s+=$1} END{print s+0}')

        if [[ $k1 == "$k2" && $p2 -gt $p1 && $k2 == "$kb" ]]; then
            echo -e "\nLive association synced ($p1 -> $p2 packets at MASTER)\n" >&2
            return 0
        fi
        echo -e "\nLive association sync pending ($p1 -> $p2 packets at MASTER).. Let's wait a little more..\n" >&2
        count=$(( $count + 1 ))
        sleep 1
    done

    echo -e "\nLive association sync failed\n" >&2
    return 1
}

# Restarts the gateway in one node with the command line it runs now, so
# the restart keeps every option the harness started it with, and with the
# same output redirect, so a gateway that dies after the restart leaves its
# stderr in /tmp/loxilb.err of that node.
function restart_llb() {
    local node=$1
    local pids cmd i ev ts

    pids=$(docker exec $node pgrep -x loxilb)
    if [[ -z $pids ]]; then
        echo "No loxilb running in $node" >&2
        return 1
    fi
    cmd=$(docker exec $node cat /proc/$(echo $pids | cut -d' ' -f1)/cmdline | tr '\0' ' ')
    # The restart truncates the redirect below, so keep the outgoing
    # gateway's output first, under the case that restarts it.
    ev=${SCTPMH_EVIDENCE:-/tmp/sctpmh-evidence}/$(basename $0 .sh)
    ts=$(date +%Y%m%dT%H%M%S)
    mkdir -p $ev
    docker cp $node:/tmp/loxilb.err $ev/$node-loxilb.err.$ts 2> /dev/null
    docker cp $node:/tmp/loxilb.out $ev/$node-loxilb.out.$ts 2> /dev/null
    echo "Killing $node loxilb pid(s): $(echo $pids)" >&2
    docker exec $node kill -9 $pids
    for i in $(seq 1 50); do
        docker exec $node pgrep -x loxilb > /dev/null || break
        sleep 0.2
    done
    if docker exec $node pgrep -x loxilb > /dev/null; then
        echo "loxilb in $node did not exit after kill" >&2
        return 1
    fi

    docker exec $node ip link del llb0 2> /dev/null
    echo "$node: $cmd" >&2
    docker exec -d $node bash -c "$cmd > /tmp/loxilb.out 2> /tmp/loxilb.err"
    for i in $(seq 1 50); do
        pids=$(docker exec $node pgrep -x loxilb)
        [[ -n $pids ]] && break
        sleep 0.2
    done
    if [[ -z $pids ]]; then
        echo "loxilb in $node did not start; /tmp/loxilb.err:" >&2
        docker exec $node tail -n 20 /tmp/loxilb.err >&2
        return 1
    fi
    echo "New loxilb pid: $(echo $pids)" >&2
}

function restart_mloxilb() {
    echo "Restarting MASTER: $master"
    restart_llb $master
}

function restart_loxilbs() {
    echo "Restarting $master"
    restart_llb $master
    echo "Restarting $backup"
    restart_llb $backup
}



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

function checkSync() {
    count=1
    sync=0
    while [[ $count -le 5 ]] ; do
        echo -e "\nStatus at MASTER:$master\n" >&2
        ct=`$dexec $master loxicmd get ct | grep est`
        echo "${ct//'\n'/$'\n'}" >&2

        echo -e "\nStatus at BACKUP:$backup\n" >&2
        ct=`$dexec $backup loxicmd get ct | grep est`
        echo "${ct//'\n'/$'\n'}" >&2

        nres1=$($hexec $master curl -sX 'GET' 'http://0.0.0.0:11111/netlox/v1/config/conntrack/all' -H 'accept: application/json' | grep -ow "\"conntrackState\":\"est\"" | wc -l)
        nres2=$($hexec $backup curl -sX 'GET' 'http://0.0.0.0:11111/netlox/v1/config/conntrack/all' -H 'accept: application/json' | grep -ow "\"conntrackState\":\"est\"" | wc -l)

        if [[ $nres1 == 0 ]]; then
            echo -e "No active connections in Master:$master. Exiting!" >&2
            return 2
        fi

        if [[ $nres1 == $nres2 && $nres1 != 0 ]]; then
            echo -e "\nConnections sync successful!!!\n" >&2
            sync=1
            break;
        fi
        echo -e "\nConnections sync pending.. Let's wait a little more..\n" >&2
        count=$(( $count + 1 ))
        sleep 2
    done

    if [[ $sync == 0 ]]; then
        echo -e "\nConnection Sync failed\n" >&2
        return 0
    fi
    echo "$sync"
}

# Restarts the gateway in one node with the command line it runs now, so
# the restart keeps every option the harness started it with, and with the
# same output redirect, so a gateway that dies after the restart leaves its
# stderr in /tmp/loxilb.err of that node.
function restart_llb() {
    local node=$1
    local pids cmd i

    pids=$(docker exec $node pgrep -x loxilb)
    if [[ -z $pids ]]; then
        echo "No loxilb running in $node" >&2
        return 1
    fi
    cmd=$(docker exec $node cat /proc/$(echo $pids | cut -d' ' -f1)/cmdline | tr '\0' ' ')
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



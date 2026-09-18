#!/bin/bash
# Exercise a Bluestone SMB share from the macOS SMB client (mount_smbfs).
#
# Usage: macos-client-test.sh SERVER[:PORT] [USER] [SHARE] [DOMAIN]
# The password is read from standard input, so it never appears in a process
# list or shell history. Work happens in a macos-test folder on the share,
# which is removed afterwards.
#
# Every step runs under a watchdog: a server that leaves the client waiting
# shows up as HUNG instead of hanging the script.

server=${1:?usage: $0 SERVER[:PORT] [USER] [SHARE] [DOMAIN]}
user=${2:-bluestone}
share=${3:-bluestone}
domain=${4:-BLUESTONE}
IFS= read -r password

# Resolve symlinks (/var is /private/var): mount reports the real path.
work=$(cd "$(mktemp -d)" && pwd -P)
mnt=$work/mnt
dir=$mnt/macos-test
mkdir "$mnt"
trap 'mount | grep -q " on $mnt " && umount "$mnt"; rm -rf "$work"' EXIT

step() {
    local label=$1; shift
    ("$@") >"$work/out" 2>&1 &
    local pid=$!
    for _ in $(seq 1 30); do kill -0 $pid 2>/dev/null || break; sleep 1; done
    if kill -0 $pid 2>/dev/null; then
        echo "HUNG: $label"; kill $pid; return 1
    fi
    wait $pid
    local rc=$?
    if [ $rc -eq 0 ]; then echo "ok    $label"; else echo "FAIL  $label (exit $rc)"; fi
    sed 's/^/      /' "$work/out" | head -8
    return $rc
}
mounted() { mount | grep -q " on $mnt "; }

# mount_smbfs takes the password in the URL, so percent-encode it.
encoded=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$password")
step "mount" mount_smbfs "//$domain;$user:$encoded@$server/$share" "$mnt"
mounted || { echo "not mounted"; exit 1; }

failed=0
run() { mounted || { echo "mount lost before: $1"; failed=1; return; }; step "$@" || failed=1; }

run "make a folder and a file" sh -c "rm -rf '$dir' && mkdir '$dir' && echo hello > '$dir/hello.txt'"
run "list"                     ls "$dir"
run "read"                     sh -c "[ \"\$(cat '$dir/hello.txt')\" = hello ]"
run "write"                    sh -c "echo from-macos > '$dir/mac.txt' && [ \"\$(cat '$dir/mac.txt')\" = from-macos ]"
run "mkdir and move into it"   sh -c "mkdir '$dir/sub' && mv '$dir/mac.txt' '$dir/sub/moved.txt' && [ \"\$(cat '$dir/sub/moved.txt')\" = from-macos ]"
run "overwrite replaces"       sh -c "echo abcdef > '$dir/over.txt' && echo xy > '$dir/over.txt' && [ \"\$(cat '$dir/over.txt')\" = xy ]"
run "5 MB copy matches"        sh -c "head -c 5000000 /dev/urandom > '$work/big.bin' && cp '$work/big.bin' '$dir/big.bin' && cmp '$work/big.bin' '$dir/big.bin'"
run "rename"                   sh -c "mv '$dir/hello.txt' '$dir/Renamed.txt' && ls '$dir' | grep -qx Renamed.txt"
run "delete"                   rm "$dir/over.txt" "$dir/big.bin"
run "clean up"                 rm -rf "$dir" "$mnt/._macos-test"
mounted && step "unmount" umount "$mnt"

exit $failed

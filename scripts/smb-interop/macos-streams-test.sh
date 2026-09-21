#!/bin/bash
# Check that macOS keeps Finder information and extended attributes on a
# Bluestone share as named streams, rather than writing AppleDouble "._"
# files beside each file.
#
# Usage: macos-streams-test.sh SERVER[:PORT] [USER] [SHARE] [DOMAIN]
# The password is read from standard input. Work happens in a
# macos-streams-test folder on the share, removed afterwards.

server=${1:?usage: $0 SERVER[:PORT] [USER] [SHARE] [DOMAIN]}
user=${2:-bluestone}
share=${3:-bluestone}
domain=${4:-BLUESTONE}
IFS= read -r password

work=$(cd "$(mktemp -d)" && pwd -P)
mnt=$work/mnt
dir=$mnt/macos-streams-test
mkdir "$mnt"
trap 'mount | grep -q " on $mnt " && umount "$mnt"; rm -rf "$work"' EXIT

encoded=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$password")
mount_smbfs "//$domain;$user:$encoded@$server/$share" "$mnt" || exit 1

check() {
    if eval "$2" >/dev/null 2>&1; then echo "ok    $1"; else echo "FAIL  $1"; failed=1; fi
}
failed=0
rm -rf "$dir" && mkdir "$dir"

# A local file carrying what Finder and browsers attach: an extended
# attribute, Finder information (32 bytes) and a Finder tag.
src=$work/tagged.txt
echo "tagged content" > "$src"
xattr -w com.example.note "kept as a stream" "$src"
xattr -wx com.apple.FinderInfo "54455854747478740000000000000000 00000000000000000000000000000000" "$src"
xattr -w com.apple.metadata:_kMDItemUserTags '("Red\n6")' "$src"

check "copy a file with attributes"      "cp '$src' '$dir/tagged.txt'"
check "content intact"                   "[ \"\$(cat '$dir/tagged.txt')\" = 'tagged content' ]"
check "extended attribute reads back"    "[ \"\$(xattr -p com.example.note '$dir/tagged.txt')\" = 'kept as a stream' ]"
check "Finder information reads back"    "xattr -px com.apple.FinderInfo '$dir/tagged.txt' | grep -q '54 45 58 54'"
check "Finder tag reads back"            "xattr -p com.apple.metadata:_kMDItemUserTags '$dir/tagged.txt' | grep -q Red"
check "set an attribute on the share"    "xattr -w com.example.later 'set in place' '$dir/tagged.txt' && [ \"\$(xattr -p com.example.later '$dir/tagged.txt')\" = 'set in place' ]"
check "remove an attribute"              "xattr -d com.example.later '$dir/tagged.txt' && ! xattr -p com.example.later '$dir/tagged.txt'"
check "attributes follow a rename"       "mv '$dir/tagged.txt' '$dir/renamed.txt' && [ \"\$(xattr -p com.example.note '$dir/renamed.txt')\" = 'kept as a stream' ]"
check "a folder takes attributes"        "mkdir '$dir/folder' && xattr -w com.example.note 'on a folder' '$dir/folder' && [ \"\$(xattr -p com.example.note '$dir/folder')\" = 'on a folder' ]"
check "no AppleDouble files"             "! ls -a '$dir' | grep -q '^\\._'"
echo "      listing: $(ls -a "$dir" | tr '\n' ' ')"

# Past the gateway's cap on a file's streams: report what happens.
big=$work/big.txt
echo big > "$big"
xattr -w com.example.big "$(head -c 4000 /dev/zero | tr '\0' 'x')" "$big"
if cp "$big" "$dir/big.txt" 2>"$work/err"; then
    echo "note  copying a file with a 4000-byte attribute succeeded; attribute kept: $(xattr -p com.example.big "$dir/big.txt" >/dev/null 2>&1 && echo yes || echo no)"
else
    echo "note  copying a file with a 4000-byte attribute failed: $(head -1 "$work/err")"
fi
echo "      listing: $(ls -a "$dir" | tr '\n' ' ')"

# BUCKET_CHECK, when set, runs before clean-up: a command listing the
# bucket's objects under macos-streams-test/, to confirm no "._" objects.
if [ -n "$BUCKET_CHECK" ]; then echo "      bucket: $(eval "$BUCKET_CHECK" | tr '\n' ' ')"; fi
rm -rf "$dir"
umount "$mnt"
exit $failed

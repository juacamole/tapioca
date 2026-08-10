#!/bin/sh
# Drive `tapioca --acp` the way an editor would, without needing an editor.
# Handshakes, opens a session in $PWD, sends one prompt, and summarizes the
# session/update notifications the editor would have rendered.
#
#   sh scripts/acp-probe.sh ["your prompt"]
set -e

PROMPT=${1:-"Use the glob tool to list the files here, then reply with just their names."}
BIN=${TAPIOCA:-tapioca}
DIR=$(mktemp -d)
PIPE=$DIR/in
LOG=$DIR/out
mkfifo "$PIPE"
trap 'exec 3>&- 2>/dev/null; kill $ACP 2>/dev/null; rm -rf "$DIR"' EXIT

"$BIN" --acp < "$PIPE" > "$LOG" 2>"$DIR/err" &
ACP=$!
exec 3> "$PIPE"
say() { printf '%s\n' "$1" >&3; }

say '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{},"clientInfo":{"name":"probe","version":"1"}}}'
say "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"session/new\",\"params\":{\"cwd\":\"$PWD\",\"mcpServers\":[]}}"

# The prompt needs the id session/new returns, so wait for it to arrive.
SID=""
i=0
while [ -z "$SID" ] && [ $i -lt 100 ]; do
	SID=$(grep -o '"sessionId":"[^"]*"' "$LOG" 2>/dev/null | tail -1 | cut -d'"' -f4)
	i=$((i + 1))
	sleep 0.2
done
if [ -z "$SID" ]; then
	echo "no session came back. agent said:"
	cat "$LOG" "$DIR/err"
	exit 1
fi
echo "session: $SID"

say "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"session/prompt\",\"params\":{\"sessionId\":\"$SID\",\"prompt\":[{\"type\":\"text\",\"text\":\"$PROMPT\"}]}}"
i=0
while [ $i -lt 600 ]; do
	grep -q '"stopReason"' "$LOG" && break
	i=$((i + 1))
	sleep 0.5
done

echo "--- what an editor would have rendered ---"
sed -n 's/.*"sessionUpdate":"\([a-z_]*\)".*/  update: \1/p' "$LOG" | sort | uniq -c
printf '  reply: '
grep -o '"stopReason":"[^"]*"' "$LOG" | tail -1
printf '  text:  '
sed -n '/agent_message_chunk/s/.*"text":"\([^"]*\)".*/\1/p' "$LOG" | tr -d '\n' | head -c 400
echo

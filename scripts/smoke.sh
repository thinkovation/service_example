#!/usr/bin/env sh
# Exercise every endpoint against a running service, whichever mode it runs in.
#
#   scripts/smoke.sh [base-url]     default: http://localhost:8080
#
# Needs only curl. Exits non-zero on the first unexpected status code, so it is
# usable as a post-deploy gate.
set -eu

BASE="${1:-http://localhost:8080}"
FAILED=0

# check NAME EXPECTED_STATUS METHOD PATH [BODY]
check() {
	name=$1 expected=$2 method=$3 path=$4
	body=${5:-}

	if [ -n "$body" ]; then
		status=$(curl -sS -o /tmp/smoke.$$ -w '%{http_code}' -X "$method" "$BASE$path" \
			-H 'Content-Type: application/json' -d "$body") || status=000
	else
		status=$(curl -sS -o /tmp/smoke.$$ -w '%{http_code}' -X "$method" "$BASE$path") || status=000
	fi

	if [ "$status" = "$expected" ]; then
		printf '  ok    %-34s %s %s\n' "$name" "$method" "$path"
	else
		printf '  FAIL  %-34s %s %s: got %s, want %s\n' "$name" "$method" "$path" "$status" "$expected"
		sed -n '1,3p' /tmp/smoke.$$
		FAILED=1
	fi
	rm -f /tmp/smoke.$$
}

# field NAME — read a top-level string field out of the last response body.
field() {
	sed -n 's/.*"'"$1"'":"\([^"]*\)".*/\1/p'
}

echo "smoke: $BASE"

check "status"                200 GET  /status
check "health"                200 GET  /zhealth

ID=$(curl -sS -X POST "$BASE/customers" -H 'Content-Type: application/json' \
	-d '{"name":"Smoke Test Co","status":"pending"}' | field id)
if [ -z "$ID" ]; then
	echo "  FAIL  create                             POST /customers: no id in response"
	exit 1
fi
printf '  ok    %-34s %s %s\n' "create" "POST" "/customers -> $ID"

check "read"                  200 GET   "/customers/$ID"
check "patch"                 200 PATCH "/customers/$ID" '{"status":"active"}'
check "replace"               200 PUT   "/customers/$ID" '{"name":"Smoke Test Company","status":"active"}'
check "list"                  200 GET   "/customers?limit=5"
check "query"                 200 QUERY /customers '{"name_contains":"smoke","limit":5}'
check "query (POST alias)"    200 POST  /customers/query '{"name_contains":"smoke","limit":5}'
check "unknown id is 404"     404 GET   "/customers/00000000-0000-7000-8000-000000000000"
check "bad id is 400"         400 GET   /customers/not-a-uuid
check "bad body is 400"       400 POST  /customers '{"name":""}'
check "unknown route is 404"  404 GET   /nope
check "bad method is 405"     405 DELETE /customers

if [ "$FAILED" -eq 0 ]; then
	echo "smoke: all checks passed"
else
	echo "smoke: failures above"
fi
exit "$FAILED"

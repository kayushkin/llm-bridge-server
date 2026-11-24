#!/usr/bin/env bash
# Score the two cmd tools' tests by sabotage rather than by asserting they are good.
#
# WHY THIS EXISTS. internal/textutil/sabotage.sh ends with a section headed
# "known gaps, reported rather than hidden", and that section says:
#
#     cmd/seed and cmd/migrate-inber call sites: NOT SCORED. Both are package
#     main with no test files, so reverting either call site to the byte cut
#     reddens nothing.
#
# That was true when it was written. This script is what closes it, so the two
# scripts have to be read together: that one scores the helper, this one scores
# the callers, and the sentence above has been struck there.
#
# HOW TO READ THE VERDICT COLUMN. This table scores MECHANISMS: each row reverts
# one piece of the fix and asks whether the suite notices.
#
#     CAUGHT    == good. The suite detects that mechanism's absence.
#     UNNOTICED == a hole, EXCEPT on a row whose expectation IS UNNOTICED, where
#                  it is required and where the row exists to prove the scorer
#                  can still return a negative.
#
# A VOID row is neither: the mutation did not apply, did not change anything, or
# did not compile. VOID is counted as not-ok, never as coverage.
#
# ⚠️ BUILD FAILURE IS NOT COVERAGE, and this is the one thing this script does
# that its sibling does not. `go test` exits non-zero when the package does not
# compile, which looks exactly like a test failing. Reverting a call site to the
# byte cut leaves textutil imported and unused, so the naive mutation does not
# fail the tests -- it fails the compiler, and a classifier that cannot tell the
# two apart scores a clean sheet while asserting nothing. Every mutation below
# that would orphan an import keeps it alive with a `_ =` line, and the
# classifier returns VOID on a build error rather than CAUGHT.
#
# Every mutation is an EXACT string replacement, not a regex, and the replacement
# asserts its search string was present and unique. A regex that goes stale
# silently mutates nothing and scores a bogus UNNOTICED.
#
# Run from anywhere. Requires a clean tree: each case restores with
# `git checkout --`, which on an uncommitted tree discards your work rather than
# the mutation.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

SOURCES=(cmd/migrate-inber/main.go cmd/seed/main.go)
if [ -n "$(git status --porcelain -- "${SOURCES[@]}")" ]; then
	echo "REFUSING: a cmd source has uncommitted changes. Commit before sabotaging --"
	echo "the per-case restore would throw your fix away, not the mutation."
	exit 1
fi

# A source is a deliberately broken version of itself from the mutate below until
# the `git checkout --` that follows it, so every way out of that window has to
# restore -- including the ways this script does not choose to take. A killed run
# left the mutated file behind as ordinary-looking uncommitted work: a semantic
# edit to a tracked source file, which `git status` reports the same way it
# reports real work in progress, and which this box's standing rule tells the next
# agent not to throw away. The refusal at the top of this script then blocks the
# next run, because of the mess the last one made.
#
# Measured by kill on this scorer before this trap existed: SIGTERM and SIGHUP
# each left cmd/migrate-inber/main.go mutated. Those are exactly what a wall-clock
# cap, systemd and a process-group kill send at an unattended run -- so the gap
# was not a corner case, it was the normal way this script gets stopped.
#
# The handler restores, clears the trap and re-raises, so the process dies BY the
# signal (rc 128+signum). A handler that restores and exits 0 tells every caller a
# killed run succeeded.
#
# SIGKILL cannot be caught by the process that receives it. It is the one gap left
# here, and it is named rather than papered over.
restore_all() { git checkout -- "${SOURCES[@]}"; }

on_signal() { # <signal name>
	restore_all
	trap - "$1"
	kill -s "$1" $$
}

for sig in INT TERM HUP; do
	# shellcheck disable=SC2064  # expand $sig now: the handler must name its own signal
	trap "on_signal $sig" "$sig"
done

pass=0
fail=0

mutate() { # <src> <find> <replace>; fails unless <find> occurs exactly once
	python3 - "$1" "$2" "$3" <<-'PY'
		import sys
		path, find, repl = sys.argv[1], sys.argv[2], sys.argv[3]
		src = open(path).read()
		n = src.count(find)
		if n != 1:
		    sys.stderr.write("search string occurs %d times, want exactly 1\n" % n)
		    sys.exit(1)
		open(path, "w").write(src.replace(find, repl))
	PY
}

classify() { # <exit_code> <output> <pkgdir>
	local code="$1" out="$2" pkgdir="$3"
	if [ "$code" -eq 0 ]; then echo "UNNOTICED"; return; fi

	# Checked before anything else. A package that does not compile reports no
	# test result at all, so reading its non-zero exit as a failing assertion
	# credits the suite for work the compiler did -- or, more often, for work
	# nobody did.
	if grep -qE '^(# |.*: )?(build failed|.*declared and not used|.*undefined:|.*imported and not used)' <<<"$out"; then
		echo "VOID (build failure -- not a test result)"
		return
	fi

	if grep -q "fixture is wrong" <<<"$out"; then
		echo "CAUGHT (guard only -- NOT coverage)"
		return
	fi

	if grep -q "^panic:" <<<"$out"; then
		# The TOP in-repo frame decides. A panic raised in source always has the
		# calling test frame below it, so "contains no _test.go frame" is never
		# true and a classifier written that way silently reports every panic as
		# fixture damage. Filter by FULL PATH: runtime/panic.go ends in .go and
		# is not _test.go, so a filename-only filter inverts the rule.
		local top
		top="$(grep -oE "/${pkgdir}/[a-z_]+\.go:[0-9]+" <<<"$out" | head -1)"
		if [ -n "$top" ] && [[ "$top" != *_test.go:* ]]; then
			echo "CAUGHT (panic in source -- coverage)"
		else
			echo "CAUGHT (panic in fixture -- NOT coverage)"
		fi
		return
	fi
	echo "CAUGHT (assertion fired)"
}

run_case() { # <name> <expect> <src> <find> <replace>
	local name="$1" expect="$2" src="$3"
	local pkgdir
	pkgdir="$(dirname "$src")"

	if ! mutate "$src" "$4" "$5" 2>/tmp/cmd-mutate.err; then
		printf '%-42s VOID -- %s' "$name" "$(cat /tmp/cmd-mutate.err)"
		fail=$((fail + 1))
		git checkout -- "$src"
		return
	fi
	if git diff --quiet -- "$src"; then
		printf '%-42s VOID -- mutation changed nothing\n' "$name"
		fail=$((fail + 1))
		git checkout -- "$src"
		return
	fi

	local out code verdict ok
	out="$(go test -count=1 "./$pkgdir" 2>&1)"
	code=$?
	verdict="$(classify "$code" "$out" "$pkgdir")"
	ok="  "
	case "$expect" in
	CAUGHT) [[ "$verdict" == CAUGHT*coverage* || "$verdict" == "CAUGHT (assertion fired)" ]] && ok="OK" ;;
	UNNOTICED) [ "$verdict" = "UNNOTICED" ] && ok="OK" ;;
	esac
	[ "$ok" = "OK" ] && pass=$((pass + 1)) || fail=$((fail + 1))
	printf '%-42s expect %-9s -> %-38s %s\n' "$name" "$expect" "$verdict" "$ok"
	git checkout -- "$src"
}

MI=cmd/migrate-inber/main.go
SEED=cmd/seed/main.go

echo "=== cmd/migrate-inber ==="

# The known-positive, and the row this whole script was written for. Until it is
# CAUGHT, every negative below is worthless.
run_case "call site reverts to a byte cut" CAUGHT "$MI" \
	'	return textutil.UpperFirstRune(id)' \
	'	_ = textutil.UpperFirstRune
	return strings.ToUpper(id[:1]) + id[1:]'

run_case "null-config check removed" CAUGHT "$MI" \
	'	if len(nullConfigs) > 0 {' \
	'	if false && len(nullConfigs) > 0 {'

run_case "null ids reported unsorted" CAUGHT "$MI" \
	'		sort.Strings(nullConfigs)' \
	'		sort.Sort(sort.Reverse(sort.StringSlice(nullConfigs)))'

run_case "configured name never overrides id" CAUGHT "$MI" \
	'	if configuredName != "" {' \
	'	if false {'

run_case "configured name always overrides id" CAUGHT "$MI" \
	'	if configuredName != "" {' \
	'	if true {'

run_case "slug ordering removed" CAUGHT "$MI" \
	'	sort.Slice(planned, func(i, j int) bool { return planned[i].Slug < planned[j].Slug })' \
	'	_ = sort.Slice'

echo
echo "=== cmd/seed ==="

run_case "call site reverts to a byte cut" CAUGHT "$SEED" \
	'	identity := agentIdentity{
		DisplayName: textutil.UpperFirstRune(ma.ID),' \
	'	_ = textutil.UpperFirstRune
	identity := agentIdentity{
		DisplayName: strings.ToUpper(ma.ID[:1]) + ma.ID[1:],'

run_case "inber nil-pointer guard removed" CAUGHT "$SEED" \
	'	if ma.Inber == nil {
		return "", false
	}
	return *ma.Inber, true' \
	'	return *ma.Inber, true'

run_case "absent inber field reported present" CAUGHT "$SEED" \
	'		return "", false' \
	'		return "", true'

run_case "override applied without a match" CAUGHT "$SEED" \
	'	if !hasInber {' \
	'	if false {'

run_case "projects list precedence dropped" CAUGHT "$SEED" \
	'	if len(ia.Projects) > 0 {' \
	'	if false {'

run_case "singular project fallback dropped" CAUGHT "$SEED" \
	'	} else if ia.Project != "" {' \
	'	} else if false {'

echo
echo "=== controls: rows that MUST come back UNNOTICED ==="

# Two known-negatives, one per package. A scorer that reports CAUGHT here
# reports CAUGHT for everything and its other rows mean nothing. Both are
# spelling changes with identical semantics, so a red would be the suite
# asserting the source text rather than the behavior.
run_case "no-op: != \"\" becomes len() != 0" UNNOTICED "$MI" \
	'	if configuredName != "" {' \
	'	if len(configuredName) != 0 {'

run_case "no-op: len()>0 becomes len()!=0" UNNOTICED "$SEED" \
	'	if len(ia.Projects) > 0 {' \
	'	if len(ia.Projects) != 0 {'

echo
echo "=== the pin on undecided behavior ==="

# This row nearly did not get written. seed's empty-inber-name blanking is
# pinned as CURRENT behavior rather than fixed, so the first instinct was that
# there is no mechanism to revert and nothing to score.
#
# That was wrong, and a per-test necessity run is what showed it: the pin test
# was the only test in either package that reddened for NO mutation, which reads
# as theater. The missing mutation is the repair itself -- guard the override the
# way cmd/migrate-inber guards it. A pin that does not redden when the pinned
# behavior changes is not pinning anything, and this is the row that proves it
# does.
#
# So CAUGHT here does not mean the mutation is wrong. It means that whoever makes
# seed agree with migrate-inber will be told by a failing test, and will have to
# close the divergence card deliberately rather than discover the change later in
# a diff of display names.
run_case "pin: empty inber name stops blanking" CAUGHT "$SEED" \
	'	identity.DisplayName = ia.Name' \
	'	if ia.Name != "" {
		identity.DisplayName = ia.Name
	}'

echo
echo "scored: $pass ok, $fail not ok"
[ "$fail" -eq 0 ] || exit 1

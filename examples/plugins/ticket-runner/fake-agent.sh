#!/bin/sh
# A fake agent: prompt, read, "work", answer. No model, no network, no key.
#
# It exists so the ticket-runner demo proves the PLUMBING rather than an LLM:
# every interesting thing in that demo — claiming a pane, waiting for a prompt,
# sending an instruction, watching frames for an answer, pushing a controller
# snapshot — happens identically whether the thing in the pane is this or a real
# agent. Swap it for `claude`, or for anything else that prints a prompt and
# answers, with --plugin's own cmd argument.
#
# The output contract with main.ts is two strings and nothing else:
#   "> "      the prompt, which means "I am reading my PTY now"
#   "DONE: X" the answer, which is what the plugin reports as the turn's result

echo "fake-agent ready — send me a ticket"

while :; do
	printf '> '
	# -r so a backslash in a ticket is data rather than an escape.
	IFS= read -r line || break
	[ -z "$line" ] && continue

	case "$line" in
	quit | exit)
		echo
		echo "bye"
		exit 0
		;;
	esac

	echo
	echo "working on: $line"
	# Long enough that a driver genuinely observes a `working` turn rather than
	# racing a transition it never sampled, and short enough to be a test.
	sleep 1
	echo "DONE: $line"
done

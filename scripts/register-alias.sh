#!/bin/sh
# scripts/register-alias.sh — install or remove the `banya` shell alias.
#
#   ./register-alias.sh <binary-path>  → add alias (idempotent)
#   ./register-alias.sh --uninstall    → remove the alias block
#
# Targets the user's shell rc files: ~/.zshrc and ~/.bashrc. Marks the
# block with BEGIN/END sentinels so we can update/remove cleanly.

set -eu

BEGIN="# >>> banya-cli alias (managed by make install) >>>"
END="# <<< banya-cli alias (managed by make install) <<<"

candidates() {
	[ -f "$HOME/.zshrc" ] && echo "$HOME/.zshrc"
	[ -f "$HOME/.bashrc" ] && echo "$HOME/.bashrc"
	[ -f "$HOME/.bash_profile" ] && echo "$HOME/.bash_profile"
}

remove_block() {
	file="$1"
	[ -f "$file" ] || return 0
	grep -qF "$BEGIN" "$file" || return 0
	tmp="$(mktemp)"
	awk -v b="$BEGIN" -v e="$END" '
		$0 == b { skip = 1; next }
		$0 == e { skip = 0; next }
		!skip   { print }
	' "$file" > "$tmp"
	mv "$tmp" "$file"
	echo "removed alias block from $file"
}

install_block() {
	file="$1"
	bin="$2"
	[ -f "$file" ] || return 0

	remove_block "$file"

	{
		printf '\n%s\n' "$BEGIN"
		# `unalias` guards against the pyenv banya shim shadowing our alias
		# if it was inherited from an earlier shell.
		echo 'unalias banya 2>/dev/null || true'
		printf 'alias banya=%s\n' "\"$bin\""
		printf '%s\n' "$END"
	} >> "$file"
	echo "registered alias in $file → $bin"
}

if [ "${1:-}" = "--uninstall" ]; then
	for rc in $(candidates); do
		remove_block "$rc"
	done
	echo "run 'source <your-rc>' or open a new terminal for changes to apply"
	exit 0
fi

if [ $# -lt 1 ]; then
	echo "usage: $0 <binary-path> | --uninstall" >&2
	exit 2
fi

BIN="$1"
if [ ! -x "$BIN" ]; then
	echo "error: not an executable: $BIN" >&2
	exit 1
fi

touched=0
for rc in $(candidates); do
	install_block "$rc" "$BIN"
	touched=$((touched + 1))
done

if [ "$touched" = 0 ]; then
	echo "no shell rc found (tried ~/.zshrc, ~/.bashrc, ~/.bash_profile)" >&2
	exit 1
fi

echo
echo "to activate now:   source ~/.zshrc   # or open a new terminal"
echo "to verify:         which banya && banya version"

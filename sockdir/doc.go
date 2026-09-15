// Package sockdir decides where magmux's IPC socket lives (--sock-dir /
// MAGMUX_SOCK_DIR), which --id names may be put into its path, and which
// sockets a magmux that died badly left behind and may be removed.
//
// It is an implementation detail of magmux, with no API stability before v1.
package sockdir

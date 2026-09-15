# r3.sed: the R3 rename map. R3 moves the leaf packages (pty, proc, sockdir,
# theme, mcp) out of package mux; this file is every identifier that changed
# name on the way, and doubles as the list of new exports (E5(c)).
#
# Dialect: sed-style s/// lines, but applied with `perl -p`, because the rules
# need \b, \u and lookarounds, which BSD sed lacks. prove-r3.sh runs it that way,
# on the PRE-R3 sources only.
#
# Each rule maps an OLD (pre-R3, package mux) spelling to the NEW spelling as
# seen from OUTSIDE the leaf, i.e. package-qualified (`theme.Pal`). The proof
# then strips the qualifiers `(theme|sockdir|pty|proc|mcp).` before a capital
# from both sides, so a name reads the same inside its own package and out.
#
# Rules are context-scoped wherever the old name is a common word (`kind`,
# `ok`, `text`, `dead`, `r`, `sockDir`): a rule that also renamed an unrelated
# struct field would show up as a proof failure or a compile error, never as a
# silent success. A few rules are scoped to one file by $ARGV (the file perl is
# reading), because the only context that tells them apart is the test they
# sit in. Order matters: field rules run before the type renames they key on.

# ── theme: palette fields ─────────────────────────────────────────────────────
# struct declaration and the two palette literals (theme.go)
s/^\t(assumedBack|bar|shadow|success|running|warn|fail|accent|text|subtle|border|ink|dead|debug)(\s+)rgb\b/\t\u$1$2rgb/;
s/^\t(assumedBack|bar|shadow|success|running|warn|fail|accent|text|subtle|border|ink|dead|debug):(\s+)rgb\{/\t\u$1:$2rgb{/;
# accesses through the palette values
s/\b(pal|darkPalette|lightPalette|tc\.p)\.(assumedBack|bar|shadow|success|running|warn|fail|accent|text|subtle|border|ink|dead|debug)\b/$1.\u$2/g;
# TestPaletteContrast's local `p := tc.p`. theme_test.go only, and never a bare
# p.dead / p.text: the same file also has `p` as a *Pane, whose dead is its own
# field. Those two are renamed only in the shapes TestPaletteContrast uses them.
s/\bp\.(assumedBack|bar|shadow|success|running|warn|fail|accent|subtle|border|ink|debug)\b/p.\u$1/g if $ARGV =~ m{(^|/)theme_test\.go$};
s/("(?:text|dead)":\s+)p\.(text|dead)\b/$1p.\u$2/g if $ARGV =~ m{(^|/)theme_test\.go$};
s/\bcontrastRatio\(p\.text, /contrastRatio(p.Text, /g if $ARGV =~ m{(^|/)theme_test\.go$};

# ── theme: rgb's fields ───────────────────────────────────────────────────────
s/^type rgb struct\{ r, g, b uint8 \}$/type rgb struct{ R, G, B uint8 }/;
s/\b(pal\.[A-Z]\w*)\.([rgb])\b/$1.\u$2/g;
s/\bc\.([rgb])\b/c.\u$1/g;
s/\b(from|to)\.([rgb])\b/$1.\u$2/g;
s/^(\t+)([rgb]): uint8\(/$1\u$2: uint8(/;

# ── theme: Inputs / ProbeResult / Resolution fields ───────────────────────────
s/^\t(flag|env|termTheme|colorFGBG)(\s+string \/\/ )/\t\u$1$2/;
s/^\t\t(env|termTheme|colorFGBG):(\s+)os\.Getenv\(/\t\t\u$1:$2os.Getenv(/;
s/^\tkind(\s+)themeKind$/\tKind$1themeKind/;
s/^\tcolor(\s+)rgb$/\tColor$1rgb/;
s/^\tok(\s+)bool$/\tOK$1bool/;
s/^\tleftover(\s+)\[\]byte$/\tLeftover$1\[\]byte/;
s/^\tsource(\s+)themeSource$/\tSource$1themeSource/;
s/^\tprobed(\s+)rgb$/\tProbed$1rgb/;
s/^\t(probedOK|probeRan)(\s+)bool$/\t\u$1$2bool/;
# single-line composite literals: capitalise the keys, leave the values alone
s#\b(themeInputs|themeProbeResult|themeResolution)\{([^{}\n]*)\}#my ($ty, $body) = ($1, $2); $body =~ s/\b(kind|color|ok|leftover|source|probed|probedOK|probeRan|flag|env|termTheme|colorFGBG):(?!=)/($1 eq "ok" ? "OK" : "\u$1") . ":"/ge; $ty . "{" . $body . "}"#ge;
# accesses
s/\bin\.(flag|env|termTheme|colorFGBG)\b/in.\u$1/g;
s/\bpr\.ok\b/pr.OK/g;
s/\bpr\.(leftover|color|kind)\b/pr.\u$1/g;
s/\b(res|zero)\.(kind|source|probed|probedOK|probeRan|leftover)\b/$1.\u$2/g;

# ── theme: rgb literals get keys ──────────────────────────────────────────────
# go vet's composites check wants a struct literal of ANOTHER package's type
# keyed. rgb is now theme.RGB, so its unkeyed literals are keyed everywhere but
# theme.go itself (whose palette literals are same-package and stay as they
# were). The second rule is xterm256's system-colour table in theme_test.go,
# whose rows elide the type: `{128, 0, 0}, {0, 128, 0}, ...`.
s/\brgb\{([^{},]+), ([^{},]+), ([^{},]+)\}/rgb{R: $1, G: $2, B: $3}/g unless $ARGV =~ m{(^|/)theme\.go$};
s/\{(\d+), (\d+), (\d+)\}/{R: $1, G: $2, B: $3}/g if $ARGV =~ m{(^|/)theme_test\.go$} && /^\t+\{\d+, \d+, \d+\}, \{/;

# ── theme: types, values and functions ────────────────────────────────────────
s/\bthemeKind\b/theme.Kind/g;
s/\bthemeDark\b/theme.Dark/g;
s/\bthemeLight\b/theme.Light/g;
s/\bthemeSource(Default|Flag|Env|TermTheme|Probe|ColorFGBG)\b/theme.Source$1/g;
s/\bthemeSource\b/theme.Source/g;
s/\bthemeInputs\b/theme.Inputs/g;
s/\bthemeProbeResult\b/theme.ProbeResult/g;
s/\bthemeResolution\b/theme.Resolution/g;
s/\bthemeProbeTimeout\b/theme.ProbeTimeout/g;
s/\bthemeEnv\b/theme.Env/g;
s/\bthemeWord\b/theme.Word/g;
s/\bvalidThemeSetting\b/theme.ValidSetting/g;
s/\bresolveTheme\b/theme.Resolve/g;
s/\bsetTheme\b/theme.Set/g;
s/\bsetDetectedBackground\b/theme.SetDetectedBackground/g;
s/\bterminalColor\b/theme.TerminalColor/g;
s/\bxColorString\b/theme.XColorString/g;
s/\bdetectThemeColor\b/theme.DetectColor/g;
s/\bclassifyOSC11\b/theme.ClassifyOSC11/g;
s/\bscreenLuminance\b/theme.ScreenLuminance/g;
s/\bparseXColor\b/theme.ParseXColor/g;
s/\bdarkPalette\b/theme.DarkPalette/g;
s/\blightPalette\b/theme.LightPalette/g;
s/\bcurrentTheme\b/theme.Current/g;
s/\btermBack\b/theme.TermBack/g;
s/\bpal\b/theme.Pal/g;
s/\bfg\(/theme.Fg(/g;
s/\bbg\(/theme.Bg(/g;
# not `rgb:` — that is the X11 colour syntax inside OSC strings, not the type
s/\brgb\b(?!:)/theme.RGB/g;
# the palette TYPE only; "palette" is also an ordinary word in a hundred comments
s/\btype palette struct\b/type theme.Palette struct/;
s/= palette\{/= theme.Palette{/g;
s/^(\t+p\s+)palette$/$1theme.Palette/;

# ── sockdir ───────────────────────────────────────────────────────────────────
# the package var, not Magmux's own sockDir field (m.sockDir, `sockDir:` keys,
# its declaration and its doc comment)
s/(?<![.\w])sockDir\b(?!:|\s+string)/sockdir.Dir/g unless /^\s*\/\/ sockDir overrides/;
s/\bvalidSockDir\b/sockdir.ValidDir/g;
s/\breapStaleSockets\b/sockdir.ReapStale/g;
s/\breapDeadline\b/sockdir.ReapDeadline/g;
s/\bvalidSocketID\b/sockdir.ValidSocketID/g;

# ── pty ───────────────────────────────────────────────────────────────────────
s/\bopenPTY\b/pty.Open/g;
s/\bsetWinSize\b/pty.SetWinSize/g;

# ── proc ──────────────────────────────────────────────────────────────────────
# not inside the error strings ("ppidOf %d: ..."), which are unchanged output
s/(?<!")\bppidOf\b/proc.PPIDOf/g;

# ── mcp ───────────────────────────────────────────────────────────────────────
s/\brunMCP\b/mcp.Run/g;

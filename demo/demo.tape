# Offline fictional PRs only. `just demo` installs the demo-local dh symlink.
# Under 40 seconds, including the real two-observation CI wait after repair.
Output docs/demo.gif

Set Shell "bash"
Set FontSize 14
Set Width 1440
Set Height 1080
Set Padding 20
Set Theme "Catppuccin Mocha"
Set TypingSpeed 25ms
Set Framerate 25

Hide
Type "export PATH=$PWD/.demo/bin:$PATH PS1='$ '; unset NO_COLOR CLICOLOR CLICOLOR_FORCE; dh verdicts forget acme/storefront acme/billing acme/gateway >/dev/null; clear"
Enter
Sleep 500ms
Show

# Bare dh intentionally shows help and the status pointer.
Type "dh"
Enter
Sleep 2s

Ctrl+L
Type "dh status"
Enter
Sleep 2s

Ctrl+L
Type "dh dependabot --open --review --apply"
Enter
Sleep 7s

Ctrl+L
Type "dh verdicts"
Enter
Sleep 3s

# The repaired PR is re-reviewed at its new head before merging.
Ctrl+L
Type "dh pr handle acme/storefront#431 --agent claude --apply"
Enter
Sleep 15s

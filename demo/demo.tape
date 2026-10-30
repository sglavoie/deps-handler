# Records docs/demo.gif against the make-believe GitHub of internal/demo:
# fictional acme repositories, no network, no real merges.
# Run with `just demo`, which builds the demo binary this puts on PATH.

Output docs/demo.gif

Set Shell "bash"
Set FontSize 14
Set Width 1440
Set Height 700
Set Padding 20
Set Theme "Catppuccin Mocha"
Set TypingSpeed 45ms

Hide
Type "export PATH=$PWD/.demo/bin:$PATH PS1='$ ' && rm -rf $HOME/Library/Caches/deps-handler-demo ${XDG_CACHE_HOME:-$HOME/.cache}/deps-handler-demo && clear"
Enter
Show

Type "deps-handler repos"
Sleep 400ms
Enter
Sleep 3s

Type "clear && deps-handler dependabot --open        # dry run: report only"
Sleep 400ms
Enter
Sleep 7s

Type "clear && deps-handler dependabot --open --review --apply"
Sleep 400ms
Enter
Sleep 9s

Type "clear && deps-handler verdicts"
Sleep 400ms
Enter
Sleep 6s

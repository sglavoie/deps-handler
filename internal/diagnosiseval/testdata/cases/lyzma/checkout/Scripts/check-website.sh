#!/bin/sh
# Fails when a page or the stylesheet refers to a local file that website/public doesn't have,
# or links to a #fragment that the target page has no id for. Runs before every deploy and on
# pull requests that touch website/ (Scripts/deploy-website.sh, .github/workflows/website.yml).
set -eu
cd "$(dirname "$0")/../website/public"

missing=0
REFS="$(mktemp)" PAGES="$(mktemp)"
trap 'rm -f "$REFS" "$PAGES"' EXIT

# A root-relative path's file: "/support/" is /support/index.html. A relative one is looked for
# beside the file that refers to it, the second argument.
file_for() {
    case "$1" in
        /*) path=".${1%%[?#]*}" ;;
        *) path="$(dirname "$2")/${1%%[?#]*}" ;;
    esac
    case "$path" in */) path="${path}index.html" ;; esac
    printf '%s\n' "$path"
}

# Pages: src, href and poster attributes, and every candidate of a srcset ("url [descriptor]",
# separated by commas). Local references, root-relative or relative, and same-page #fragments are
# checked; other sites' (a scheme or //) aren't.
find . -name '*.html' >"$PAGES"
while IFS= read -r page; do
    {
        grep -oE '(src|href|poster)="[^"]*"' "$page" | sed -E 's/^[a-z]+="//; s/"$//'
        grep -oE 'srcset="[^"]*"' "$page" | sed -E 's/^srcset="//; s/"$//' | tr ',' '\n' | awk 'NF { print $1 }'
    } >"$REFS" || true
    while IFS= read -r ref; do
        case "$ref" in
            "" | //* | *:*) continue ;;
            "#"*) target="$page" ;;
            *) target="$(file_for "$ref" "$page")" ;;
        esac
        if [ ! -f "$target" ]; then
            echo "Missing: $ref (in $page)" >&2
            missing=1
            continue
        fi
        case "$ref" in
            *"#"?*)
                fragment="${ref#*#}"
                case "$target" in
                    *.html)
                        if ! grep -qF "id=\"$fragment\"" "$target"; then
                            echo "No id \"$fragment\" in $target: $ref (in $page)" >&2
                            missing=1
                        fi
                        ;;
                esac
                ;;
        esac
    done <"$REFS"
done <"$PAGES"

# The stylesheets' url()s.
find . -name '*.css' >"$PAGES"
while IFS= read -r sheet; do
    grep -oE 'url\("[^"]*"\)' "$sheet" | sed -E 's/^url\("//; s/"\)$//' >"$REFS" || true
    while IFS= read -r ref; do
        case "$ref" in "" | //* | *:*) continue ;; esac
        if [ ! -f "$(file_for "$ref" "$sheet")" ]; then
            echo "Missing: $ref (in $sheet)" >&2
            missing=1
        fi
    done <"$REFS"
done <"$PAGES"

[ "$missing" = 0 ] && echo "Every local file and #fragment the website refers to exists."
exit "$missing"

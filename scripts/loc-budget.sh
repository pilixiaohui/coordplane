#!/bin/sh
set -eu

check=false
output=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --check) check=true ;;
    --output)
      shift
      [ "$#" -gt 0 ] || { echo "loc-budget: --output requires a path" >&2; exit 2; }
      output=$1
      ;;
    *) echo "loc-budget: unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
[ -n "$output" ] || { echo "loc-budget: --output is required" >&2; exit 2; }

if [ "${output#/}" = "$output" ]; then
  output=$PWD/$output
fi

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
git -C "$root" rev-parse --is-inside-work-tree >/dev/null
cd "$root"
manifest=scripts/loc-manifest.tsv
generated_manifest=scripts/generated-manifest.json
[ -f "$manifest" ] && [ -f "$generated_manifest" ] || {
  echo "loc-budget: manifests are missing" >&2
  exit 2
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/coordplane-loc.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
for report in records unknown file-warnings file-blockers function-warnings function-blockers gofmt multistatement-blockers go-files untracked-maintained; do
  : >"$tmp/$report"
done

classify() {
  classify_path=$1
  while IFS='|' read -r pattern bucket module; do
    case "$pattern" in ''|'#'*) continue ;; esac
    case "$classify_path" in
      $pattern)
        printf '%s|%s\n' "$bucket" "$module"
        return 0
        ;;
    esac
  done <"$manifest"
  return 1
}

count_lines() {
  awk -v go="$2" '
    function trim(s) { sub(/^[[:space:]]+/, "", s); return s }
    {
      s=trim($0)
      if (s=="") next
      if (!go) {
        if (s ~ /^#/ && s !~ /^#!/) next
        if (s ~ /^\/\//) next
        total++
        next
      }
      while (1) {
        if (block) {
          if (match(s, /\*\//)) { s=trim(substr(s,RSTART+RLENGTH)); block=0; if (s=="") next }
          else next
        }
        if (s ~ /^\/\//) next
        if (s ~ /^\/\*/) {
          if (match(s, /\*\//)) { s=trim(substr(s,RSTART+RLENGTH)); if (s=="") next; continue }
          block=1
          next
        }
        total++
        next
      }
    }
    END { print total+0 }
  ' "$1"
}

check_functions() {
  awk -v path="$1" -v warnings="$tmp/function-warnings" -v blockers="$tmp/function-blockers" '
    /^[[:space:]]*func[[:space:]]/ {
      active=1; seen=0; depth=0; lines=0; start=FNR
    }
    active {
      line=$0
      sub(/^[[:space:]]+/, "", line)
      if (line!="" && line!~/^\/\//) lines++
      braces=$0
      gsub(/"([^"\\]|\\.)*"/, "", braces)
      d=gsub(/{/, "{", braces)-gsub(/}/, "}", braces)
      if (d>0) seen=1
      depth+=d
      if (seen && depth<=0) {
        item=path ":" start ":" lines
        if (lines>140) print item >> blockers
        else if (lines>80) print item >> warnings
        active=0
      }
    }
  ' "$1"
}

git ls-files --cached --others --exclude-standard | LC_ALL=C sort -u | while IFS= read -r path; do
  [ -f "$path" ] || continue
  if ! classification=$(classify "$path"); then
    printf '%s\n' "$path" >>"$tmp/unknown"
    continue
  fi
  bucket=${classification%%|*}
  module=${classification#*|}
  [ "$bucket" != excluded ] || continue
  if ! git ls-files --error-unmatch "$path" >/dev/null 2>&1; then
    printf '%s\n' "$path" >>"$tmp/untracked-maintained"
  fi
  case "$path" in
    *.go) lines=$(count_lines "$path" 1); printf '%s\n' "$path" >>"$tmp/go-files" ;;
    *) lines=$(count_lines "$path" 0) ;;
  esac
  case "$bucket" in
    generated_*)
      grep -Fq "\"path\": \"$path\"" "$generated_manifest" || printf '%s\n' "$path" >>"$tmp/unknown"
      ;;
    *)
      if grep -q 'Code generated .* DO NOT EDIT\.' "$path"; then
        printf '%s\n' "$path" >>"$tmp/unknown"
      fi
      ;;
  esac
  printf '%s|%s|%s|%s\n' "$bucket" "$module" "$lines" "$path" >>"$tmp/records"
  if [ "$bucket" = handwritten_production ] || [ "$bucket" = generated_semantic_production ]; then
    if [ "$lines" -gt 800 ]; then printf '%s:%s\n' "$path" "$lines" >>"$tmp/file-blockers"
    elif [ "$lines" -gt 500 ]; then printf '%s:%s\n' "$path" "$lines" >>"$tmp/file-warnings"
    fi
    case "$path" in *.go) check_functions "$path" ;; esac
  fi
done
go run ./scripts/locguard <"$tmp/go-files" >"$tmp/multistatement-blockers"

# The pipeline loop runs in a subshell; fixture bytes are calculated separately.
fixture_bytes=$(git ls-files --cached --others --exclude-standard | awk '/(^|\/)testdata\//{print}' | xargs -r wc -c | awk 'END{print $1+0}')
gofmt -l $(git ls-files --cached --others --exclude-standard '*.go') >"$tmp/gofmt"

base=${LOC_BASE_REF:-HEAD^}
git rev-parse --verify "$base^{commit}" >/dev/null 2>&1 || base=$(git rev-list --max-parents=0 HEAD)
base=$(git rev-parse "$base^{commit}")
revision=$(git rev-parse HEAD)
raw_added=$(git diff --numstat "$base" -- | awk '$1~/^[0-9]+$/{n+=$1} END{print n+0}')
raw_deleted=$(git diff --numstat "$base" -- | awk '$2~/^[0-9]+$/{n+=$2} END{print n+0}')
raw_added=$((raw_added + $(git ls-files --others --exclude-standard | xargs -r wc -l | awk 'END{print $1+0}')))

handwritten_production=$(awk -F'|' '$1=="handwritten_production"{n+=$3} END{print n+0}' "$tmp/records")
handwritten_tests=$(awk -F'|' '$1=="handwritten_tests"{n+=$3} END{print n+0}' "$tmp/records")
handwritten_infra=$(awk -F'|' '$1=="handwritten_infra"{n+=$3} END{print n+0}' "$tmp/records")
generated_semantic_production=$(awk -F'|' '$1=="generated_semantic_production"{n+=$3} END{print n+0}' "$tmp/records")
generated_semantic_tests=$(awk -F'|' '$1=="generated_semantic_tests"{n+=$3} END{print n+0}' "$tmp/records")
generated_semantic_infra=$(awk -F'|' '$1=="generated_semantic_infra"{n+=$3} END{print n+0}' "$tmp/records")
generated_mechanical_excluded=$(awk -F'|' '$1=="generated_mechanical_excluded"{n+=$3} END{print n+0}' "$tmp/records")
production=$((handwritten_production + generated_semantic_production))
tests=$((handwritten_tests + generated_semantic_tests))
infra=$((handwritten_infra + generated_semantic_infra))
total=$((production + tests + infra))
generated_total=$((generated_semantic_production + generated_semantic_tests + generated_semantic_infra + generated_mechanical_excluded))

failure=false
[ "$production" -le 20500 ] || failure=true
[ "$tests" -le 22500 ] || failure=true
[ "$infra" -le 600 ] || failure=true
[ "$total" -le 43600 ] || failure=true
[ "$generated_total" -le 3000 ] || failure=true
for report in unknown file-blockers function-blockers gofmt multistatement-blockers; do
  [ ! -s "$tmp/$report" ] || failure=true
done
clean=true
git diff --quiet -- && git diff --cached --quiet -- || clean=false
[ ! -s "$tmp/untracked-maintained" ] || clean=false
[ "$clean" = true ] || failure=true

awk -F'|' \
  -v revision="$revision" -v base="$base" -v added="$raw_added" -v deleted="$raw_deleted" \
  -v hp="$handwritten_production" -v ht="$handwritten_tests" -v hi="$handwritten_infra" \
  -v gsp="$generated_semantic_production" -v gst="$generated_semantic_tests" -v gsi="$generated_semantic_infra" -v gme="$generated_mechanical_excluded" \
  -v production="$production" -v tests="$tests" -v infra="$infra" -v total="$total" \
  -v generated="$generated_total" -v source_total="$((total + generated_mechanical_excluded))" -v fixture_bytes="$fixture_bytes" \
  -v unknown="$tmp/unknown" -v fw="$tmp/file-warnings" -v fb="$tmp/file-blockers" -v fnw="$tmp/function-warnings" -v fnb="$tmp/function-blockers" -v gofmt_bad="$tmp/gofmt" -v multi="$tmp/multistatement-blockers" -v clean="$clean" -v failed="$failure" '
  function esc(s) { gsub(/\\/, "\\\\", s); gsub(/"/, "\\\"", s); return s }
  function array(path, line, first) {
    printf "["; first=1
    while ((getline line < path)>0) { if(!first) printf ","; printf "\"%s\"", esc(line); first=0 }
    close(path); printf "]"
  }
  { module[$1 SUBSEP $2]+=$3 }
  END {
    printf "{\n  \"schema_version\": 1,\n  \"revision\": \"%s\",\n  \"base_revision\": \"%s\",\n", revision, base
    printf "  \"atomic_buckets\": {\"handwritten_production\":%d,\"handwritten_tests\":%d,\"handwritten_infra\":%d,\"generated_semantic_production\":%d,\"generated_semantic_tests\":%d,\"generated_semantic_infra\":%d,\"generated_mechanical_excluded\":%d},\n", hp,ht,hi,gsp,gst,gsi,gme
    printf "  \"budgeted\": {\"production\":%d,\"tests\":%d,\"infra\":%d,\"total\":%d},\n", production,tests,infra,total
    printf "  \"thresholds\": {\"production\":20500,\"tests\":22500,\"infra\":600,\"total\":43600,\"generated_review\":3000},\n"
    printf "  \"generated_total\": %d,\n  \"first_party_source_total\": %d,\n  \"fixture_bytes\": %d,\n", generated,source_total,fixture_bytes
    printf "  \"modules\": {"; n=asorti(module, keys); for(i=1;i<=n;i++){split(keys[i],p,SUBSEP); if(i>1)printf ","; printf "\"%s/%s\":%d",esc(p[1]),esc(p[2]),module[keys[i]]} printf "},\n"
    printf "  \"diff\": {\"raw_added\":%d,\"raw_deleted\":%d},\n", added,deleted
    printf "  \"quality\": {\"unknown_paths\":"; array(unknown); printf ",\"file_warnings\":"; array(fw); printf ",\"file_blockers\":"; array(fb); printf ",\"function_warnings\":"; array(fnw); printf ",\"function_blockers\":"; array(fnb); printf ",\"gofmt_files\":"; array(gofmt_bad); printf ",\"multistatement_blockers\":"; array(multi); printf "},\n"
    printf "  \"clean_revision\": %s,\n", clean=="true" ? "true" : "false"
    printf "  \"pass\": %s\n}\n", failed=="true" ? "false" : "true"
  }
' "$tmp/records" >"$tmp/report.json"

mkdir -p "$(dirname "$output")"
cp "$tmp/report.json" "$output.tmp.$$"
mv "$output.tmp.$$" "$output"
result=PASS
[ "$failure" = false ] || result=FAIL
echo "loc-budget: $result production=$production tests=$tests infra=$infra total=$total (report: $output)" >&2
[ "$failure" = false ] || [ "$check" = false ] || exit 1

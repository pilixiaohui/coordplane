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

case "$output" in
  /*) ;;
  *) output=$PWD/$output ;;
esac

root=$(git rev-parse --show-toplevel)
cd "$root"
manifest=scripts/loc-manifest.tsv
generated_manifest=scripts/generated-manifest.json
[ -f "$manifest" ] && [ -f "$generated_manifest" ] || {
  echo "loc-budget: manifests are missing" >&2
  exit 2
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/coordplane-loc.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
records=$tmp/records
unknown=$tmp/unknown
file_warnings=$tmp/file-warnings
file_blockers=$tmp/file-blockers
function_warnings=$tmp/function-warnings
function_blockers=$tmp/function-blockers
gofmt_bad=$tmp/gofmt
: >"$records"
: >"$unknown"
: >"$file_warnings"
: >"$file_blockers"
: >"$function_warnings"
: >"$function_blockers"

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

count_go() {
  awk '
    function trim(s) { sub(/^[[:space:]]+/, "", s); return s }
    {
      s=trim($0)
      if (s=="") next
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

count_text() {
  awk '
    {
      s=$0
      sub(/^[[:space:]]+/, "", s)
      if (s=="") next
      if (s ~ /^#/ && s !~ /^#!/) next
      if (s ~ /^\/\//) next
      total++
    }
    END { print total+0 }
  ' "$1"
}

check_functions() {
  awk -v path="$1" -v warnings="$function_warnings" -v blockers="$function_blockers" '
    function code(s) {
      sub(/^[[:space:]]+/, "", s)
      return s!="" && s!~/^\/\//
    }
    function delta(s, opens, closes) {
      gsub(/"([^"\\]|\\.)*"/, "", s)
      opens=gsub(/{/, "{", s)
      closes=gsub(/}/, "}", s)
      return opens-closes
    }
    /^[[:space:]]*func[[:space:]]/ {
      active=1; seen=0; depth=0; lines=0; start=FNR
    }
    active {
      if (code($0)) lines++
      d=delta($0)
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

fixture_bytes=0
git ls-files --cached --others --exclude-standard | LC_ALL=C sort -u | while IFS= read -r path; do
  [ -f "$path" ] || continue
  if ! classification=$(classify "$path"); then
    printf '%s\n' "$path" >>"$unknown"
    continue
  fi
  bucket=${classification%%|*}
  module=${classification#*|}
  [ "$bucket" != excluded ] || continue
  case "$path" in *.go) lines=$(count_go "$path") ;; *) lines=$(count_text "$path") ;; esac
  case "$bucket" in
    generated_*)
      grep -Fq "\"path\": \"$path\"" "$generated_manifest" || printf '%s\n' "$path" >>"$unknown"
      ;;
    *)
      if grep -q 'Code generated .* DO NOT EDIT\.' "$path"; then
        printf '%s\n' "$path" >>"$unknown"
      fi
      ;;
  esac
  printf '%s|%s|%s|%s\n' "$bucket" "$module" "$lines" "$path" >>"$records"
  if [ "$bucket" = handwritten_production ] || [ "$bucket" = generated_semantic_production ]; then
    if [ "$lines" -gt 800 ]; then printf '%s:%s\n' "$path" "$lines" >>"$file_blockers"
    elif [ "$lines" -gt 500 ]; then printf '%s:%s\n' "$path" "$lines" >>"$file_warnings"
    fi
    case "$path" in *.go) check_functions "$path" ;; esac
  fi
done

# The pipeline loop runs in a subshell; fixture bytes are calculated separately.
fixture_bytes=$(git ls-files --cached --others --exclude-standard | awk '/(^|\/)testdata\//{print}' | xargs -r wc -c | awk 'END{print $1+0}')
gofmt -l $(git ls-files --cached --others --exclude-standard '*.go') >"$gofmt_bad"

base=${LOC_BASE_REF:-HEAD^}
git rev-parse --verify "$base^{commit}" >/dev/null 2>&1 || base=$(git rev-list --max-parents=0 HEAD)
base=$(git rev-parse "$base^{commit}")
revision=$(git rev-parse HEAD)
raw_added=$(git diff --numstat "$base" -- | awk '$1~/^[0-9]+$/{n+=$1} END{print n+0}')
raw_deleted=$(git diff --numstat "$base" -- | awk '$2~/^[0-9]+$/{n+=$2} END{print n+0}')
untracked_added=$(git ls-files --others --exclude-standard | xargs -r wc -l | awk 'END{print $1+0}')
raw_added=$((raw_added + untracked_added))

handwritten_production=$(awk -F'|' '$1=="handwritten_production"{n+=$3} END{print n+0}' "$records")
handwritten_tests=$(awk -F'|' '$1=="handwritten_tests"{n+=$3} END{print n+0}' "$records")
handwritten_infra=$(awk -F'|' '$1=="handwritten_infra"{n+=$3} END{print n+0}' "$records")
generated_semantic_production=$(awk -F'|' '$1=="generated_semantic_production"{n+=$3} END{print n+0}' "$records")
generated_semantic_tests=$(awk -F'|' '$1=="generated_semantic_tests"{n+=$3} END{print n+0}' "$records")
generated_semantic_infra=$(awk -F'|' '$1=="generated_semantic_infra"{n+=$3} END{print n+0}' "$records")
generated_mechanical_excluded=$(awk -F'|' '$1=="generated_mechanical_excluded"{n+=$3} END{print n+0}' "$records")
production=$((handwritten_production + generated_semantic_production))
tests=$((handwritten_tests + generated_semantic_tests))
infra=$((handwritten_infra + generated_semantic_infra))
total=$((production + tests + infra))
generated_total=$((generated_semantic_production + generated_semantic_tests + generated_semantic_infra + generated_mechanical_excluded))
first_party_source_total=$((total + generated_mechanical_excluded))

failure=false
[ "$production" -le 14650 ] || failure=true
[ "$tests" -le 19000 ] || failure=true
[ "$infra" -le 600 ] || failure=true
[ "$total" -le 34250 ] || failure=true
[ "$generated_total" -le 3000 ] || failure=true
[ ! -s "$unknown" ] || failure=true
[ ! -s "$file_blockers" ] || failure=true
[ ! -s "$function_blockers" ] || failure=true
[ ! -s "$gofmt_bad" ] || failure=true

json_tmp=$tmp/report.json
awk -F'|' \
  -v revision="$revision" -v base="$base" -v added="$raw_added" -v deleted="$raw_deleted" \
  -v hp="$handwritten_production" -v ht="$handwritten_tests" -v hi="$handwritten_infra" \
  -v gsp="$generated_semantic_production" -v gst="$generated_semantic_tests" -v gsi="$generated_semantic_infra" -v gme="$generated_mechanical_excluded" \
  -v production="$production" -v tests="$tests" -v infra="$infra" -v total="$total" \
  -v generated="$generated_total" -v source_total="$first_party_source_total" -v fixture_bytes="$fixture_bytes" \
  -v unknown="$unknown" -v fw="$file_warnings" -v fb="$file_blockers" -v fnw="$function_warnings" -v fnb="$function_blockers" -v gofmt_bad="$gofmt_bad" -v failed="$failure" '
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
    printf "  \"thresholds\": {\"production\":14650,\"tests\":19000,\"infra\":600,\"total\":34250,\"generated_review\":3000},\n"
    printf "  \"generated_total\": %d,\n  \"first_party_source_total\": %d,\n  \"fixture_bytes\": %d,\n", generated,source_total,fixture_bytes
    printf "  \"modules\": {"; n=asorti(module, keys); for(i=1;i<=n;i++){split(keys[i],p,SUBSEP); if(i>1)printf ","; printf "\"%s/%s\":%d",esc(p[1]),esc(p[2]),module[keys[i]]} printf "},\n"
    printf "  \"diff\": {\"raw_added\":%d,\"raw_deleted\":%d},\n", added,deleted
    printf "  \"quality\": {\"unknown_paths\":"; array(unknown); printf ",\"file_warnings\":"; array(fw); printf ",\"file_blockers\":"; array(fb); printf ",\"function_warnings\":"; array(fnw); printf ",\"function_blockers\":"; array(fnb); printf ",\"gofmt_files\":"; array(gofmt_bad); printf "},\n"
    printf "  \"pass\": %s\n}\n", failed=="true" ? "false" : "true"
  }
' "$records" >"$json_tmp"

mkdir -p "$(dirname "$output")"
cp "$json_tmp" "$output.tmp.$$"
mv "$output.tmp.$$" "$output"
if [ "$failure" = true ]; then
  echo "loc-budget: FAIL production=$production tests=$tests infra=$infra total=$total (report: $output)" >&2
  [ "$check" = false ] || exit 1
else
  echo "loc-budget: PASS production=$production tests=$tests infra=$infra total=$total (report: $output)" >&2
fi

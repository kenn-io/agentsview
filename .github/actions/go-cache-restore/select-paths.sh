paths=()
modules="$(go env GOMODCACHE)"
while IFS= read -r path; do
  path="${path/#\~/$HOME}"
  if [ "$path" = "$modules" ]; then continue; fi
  if [ "$RUNNER_OS" = 'Windows' ]; then path="$(cygpath -u "$path")"; fi
  if [ -d "$path" ]; then paths+=("$path"); fi
done <<< "$GO_CACHE_PATHS"

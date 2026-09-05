#!/usr/bin/env bash

set -euo pipefail

simple_repo="https://github.com/wangfenjin/simple.git"
simple_commit="4ed008934495fc55ff4bf6620bba58311988b23e"
cppjieba_commit="194c144d8b5ed1baf3190d07c5226e804454ab47"
output_dir="${1:-dist/agentsview-simple}"
temp_parent="${TMPDIR:-${XDG_CACHE_HOME:-$HOME/.cache}}"

mkdir -p "$temp_parent"
build_root=$(mktemp -d "$temp_parent/agentsview-simple-build.XXXXXX")
cleanup() {
    rm -rf -- "$build_root"
}
trap cleanup EXIT HUP INT TERM

git clone --quiet --filter=blob:none --no-checkout "$simple_repo" "$build_root/source"
git -C "$build_root/source" checkout --quiet --detach "$simple_commit"
actual_commit=$(git -C "$build_root/source" rev-parse HEAD)
if [ "$actual_commit" != "$simple_commit" ]; then
    echo "simple checkout mismatch: expected $simple_commit, got $actual_commit" >&2
    exit 1
fi

linker_flags=""
case "$(uname -s)" in
    Linux) linker_flags="-static-libstdc++ -static-libgcc" ;;
    Darwin) ;;
    MINGW*|MSYS*|CYGWIN*) ;;
    *) echo "unsupported platform: $(uname -s)" >&2; exit 1 ;;
esac

cmake -S "$build_root/source" -B "$build_root/build" \
    -DBUILD_SQLITE3=OFF \
    -DBUILD_TEST_EXAMPLE=OFF \
    -DSIMPLE_WITH_JIEBA=ON \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_SHARED_LINKER_FLAGS="$linker_flags"
cmake --build "$build_root/build" --config Release --parallel

cppjieba_dir="$build_root/build/cppjieba/src/cppjieba"
actual_cppjieba=$(git -C "$cppjieba_dir" rev-parse HEAD)
if [ "$actual_cppjieba" != "$cppjieba_commit" ]; then
    echo "cppjieba checkout mismatch: expected $cppjieba_commit, got $actual_cppjieba" >&2
    exit 1
fi

case "$(uname -s)" in
    Linux)
        built_library="$build_root/build/src/libsimple.so"
        installed_library="libsimple.so"
        ;;
    Darwin)
        built_library="$build_root/build/src/libsimple.dylib"
        installed_library="libsimple.dylib"
        ;;
    MINGW*|MSYS*|CYGWIN*)
        built_library="$build_root/build/src/Release/simple.dll"
        if [ ! -f "$built_library" ]; then
            built_library="$build_root/build/src/libsimple.dll"
        fi
        installed_library="simple.dll"
        ;;
esac

if [ ! -f "$built_library" ]; then
    echo "built simple library not found at $built_library" >&2
    exit 1
fi

rm -rf -- "$output_dir"
mkdir -p "$output_dir/dict" "$output_dir/licenses"
install -m 0755 "$built_library" "$output_dir/$installed_library"
for name in hmm_model.utf8 idf.utf8 jieba.dict.utf8 stop_words.utf8 user.dict.utf8; do
    install -m 0644 "$cppjieba_dir/dict/$name" "$output_dir/dict/$name"
done
install -m 0644 "$build_root/source/LICENSE" "$output_dir/licenses/simple-LICENSE"
install -m 0644 "$cppjieba_dir/LICENSE" "$output_dir/licenses/cppjieba-LICENSE"
printf '%s\n' \
    "simple $simple_commit" \
    "cppjieba $cppjieba_commit" \
    > "$output_dir/VERSIONS"

echo "Built $output_dir"

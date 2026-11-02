#!/bin/sh
# Compiles mobile/sample against the built .aar (dist/bangdb-*.aar) and the
# Android platform jar, so API changes that break callers fail CI.
set -eu
cd "$(dirname "$0")/.."
aar=$(ls dist/bangdb-*.aar | head -n 1)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
unzip -q "$aar" classes.jar -d "$tmp"
platform=$(ls -d "${ANDROID_HOME:-$ANDROID_SDK_ROOT}"/platforms/android-* | sort -V | tail -n 1)
cp="$tmp/classes.jar:$platform/android.jar"
javac -d "$tmp/java" -cp "$cp" mobile/sample/NoteStoreJava.java
echo "Java sample compiles against $aar"
if command -v kotlinc >/dev/null 2>&1; then
	kotlinc -cp "$cp" -d "$tmp/kotlin.jar" mobile/sample/NoteStore.kt
	echo "Kotlin sample compiles"
else
	echo "kotlinc not found: Kotlin sample not compiled" >&2
fi

#!/bin/sh

set -eu

if [ "$#" -eq 0 ]; then
	echo "usage: EPUBCHECK_JAR=/path/to/epubcheck.jar $0 BOOK.{pdf,epub,cbz} [...]" >&2
	exit 2
fi

validation_tmp=$(mktemp -d "${TMPDIR:-/tmp}/leafport-validation.XXXXXX")
cleanup() {
	case "$validation_tmp" in
		*/leafport-validation.*) rm -rf -- "$validation_tmp" ;;
	esac
}
trap cleanup EXIT HUP INT TERM

need_command() {
	if ! command -v "$1" >/dev/null 2>&1; then
		echo "missing required validator: $1" >&2
		exit 2
	fi
}

leafport_validate() {
	if [ -n "${LEAFPORT_BIN:-}" ]; then
		"$LEAFPORT_BIN" validate --input "$1"
	else
		need_command go
		go run . validate --input "$1"
	fi
}

validate_epub() {
	publication=$1
	need_command mutool
	if [ -z "${EPUBCHECK_JAR:-}" ] || [ ! -f "$EPUBCHECK_JAR" ]; then
		echo "EPUBCHECK_JAR must name the official EPUBCheck JAR" >&2
		exit 2
	fi
	if [ -n "${JAVA:-}" ]; then
		if [ ! -x "$JAVA" ]; then
			echo "JAVA is not executable: $JAVA" >&2
			exit 2
		fi
		java_command=$JAVA
	else
		need_command java
		java_command=java
	fi
	"$java_command" -jar "$EPUBCHECK_JAR" --failonwarnings "$publication"
	mutool draw -q -F png -r 24 -o /dev/null "$publication"

	if [ -n "${KINDLEGEN:-}" ]; then
		if [ ! -x "$KINDLEGEN" ]; then
			echo "KINDLEGEN is not executable: $KINDLEGEN" >&2
			exit 2
		fi
		kindle_dir=$(mktemp -d "$validation_tmp/kindlegen.XXXXXX")
		cp "$publication" "$kindle_dir/book.epub"
		"$KINDLEGEN" "$kindle_dir/book.epub" -o book.mobi
		test -s "$kindle_dir/book.mobi"
	fi

	if [ -n "${EBOOK_CONVERT:-}" ]; then
		if [ ! -x "$EBOOK_CONVERT" ]; then
			echo "EBOOK_CONVERT is not executable: $EBOOK_CONVERT" >&2
			exit 2
		fi
		calibre_dir=$(mktemp -d "$validation_tmp/calibre.XXXXXX")
		CALIBRE_CONFIG_DIRECTORY="$calibre_dir/config" \
			"$EBOOK_CONVERT" "$publication" "$calibre_dir/book.azw3"
		test -s "$calibre_dir/book.azw3"
	fi
}

validate_pdf() {
	publication=$1
	need_command qpdf
	need_command gs
	need_command mutool
	if [ -z "${PDFCPU:-}" ] || [ ! -x "$PDFCPU" ]; then
		echo "PDFCPU must name an executable pdfcpu binary" >&2
		exit 2
	fi
	"$PDFCPU" validate -m strict -o "$publication"
	qpdf --check "$publication"
	gs -q -dSAFER -dNOPAUSE -dBATCH -sDEVICE=nullpage "$publication"
	mutool draw -q -F png -r 36 -o /dev/null "$publication"
	mutool draw -q -R 90 -F png -r 36 -o /dev/null "$publication"
}

validate_cbz() {
	need_command unzip
	unzip -t "$1"
}

sequence=0
for publication in "$@"; do
	sequence=$((sequence + 1))
	if [ ! -f "$publication" ]; then
		echo "publication does not exist: $publication" >&2
		exit 2
	fi
	echo "[$sequence/$#] $publication"
	leafport_validate "$publication"
	case "$publication" in
		*.epub|*.EPUB) validate_epub "$publication" ;;
		*.pdf|*.PDF) validate_pdf "$publication" ;;
		*.cbz|*.CBZ) validate_cbz "$publication" ;;
		*)
			echo "unsupported publication extension: $publication" >&2
			exit 2
			;;
	esac
done

echo "All publication validation gates passed."

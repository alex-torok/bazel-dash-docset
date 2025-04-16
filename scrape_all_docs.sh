set -euo pipefail

function main {
    scrape_latest
    scrape_version 8.0.0
    scrape_version 7.6.0
    scrape_version 7.5.0
    scrape_version 7.4.0
    scrape_version 7.0.0
    scrape_version 6.5.0
}

function scrape_version {
    version="$1"
    outdir="versions/$version"
    mkdir -p $outdir

    # TODO: Figure out how to ignore links to other verions?
    # How do you keep it under a specific path?
    httrack \
        "https://bazel.build/versions/$version" \
        -O "$outdir" \
        --sockets=20 \
        "+fonts.googleapis.com/*" \
        "+*.gstatic.com/*" \
        "-bazel.build/*" \
        "+bazel.build/_pwa/*" \
        "+bazel.build/versions/$version/*" \
        "+bazel.build/*.css" \
        "-*?hl=*"

    fix_links $outdir
}

function fix_links {
    find $1 -type f -name '*.html' | xargs sed -i 's/-2.html/.html/'
    find $1 -type f -name '*-2.html' -delete
}

function scrape_latest {
    outdir="versions/latest"
    mkdir -p $outdir

    httrack \
        "https://bazel.build" \
        -O "$outdir" \
        --sockets=20 \
        "+fonts.googleapis.com/*" \
        "+*.gstatic.com/*" \
        "-*?hl=*" \
        "-*bazel.build/versions/*"
}

main

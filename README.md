# Dash Bazel Docs Gen

This repo contains the code necessary for generating [Dash](https://kapeli.com/dash)
docsets for bazel. The code for creating the docset started as a fork of
[Dashing](https://github.com/technosophos/dashing)

## Running it locally

**Install Requirements**

The docset generator requires `httrack` to download the docs and a working golang installation
to generate the docset.

```
brew install httrack
```

The scripts expect that you have a clone of the `Dash-User-Contributions` repo
in a sibling directory of this repo's clone.

**Scrape the site and generate all versions of docs**

```
./scrape_all_docs.sh
./build_all_docs.sh
```

## How it works

Bazel's docs are versioned in the [bazel
repo](https://github.com/bazelbuild/bazel) and consist of a mixture of
automatically-generated HTML and user-written markdown files.

The site is published with google's internal devsite infrastructure, so
it is not possible to build the HTML for the site entirely locally. To
get around that limitation, we use the [open-source `httrack`
CLI](https://www.httrack.com/) to archive the site and all of its dependencies
locally.

Once the site is archived locally, the go tool is used to crawl the html files
and build up the dash docset.

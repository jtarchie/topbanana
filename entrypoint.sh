#!/bin/sh
# Tigris (fly storage create) exports BUCKET_NAME + AWS_ENDPOINT_URL_S3; the
# Go binary reads S3_BUCKET + AWS_ENDPOINT_URL. Alias them here so the rest
# of the stack just works.
set -e
export S3_BUCKET="${S3_BUCKET:-$BUCKET_NAME}"
export AWS_ENDPOINT_URL="${AWS_ENDPOINT_URL:-$AWS_ENDPOINT_URL_S3}"
exec /usr/local/bin/topbanana "$@"

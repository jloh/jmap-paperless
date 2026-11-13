# Built by goreleaser, which provides the prebuilt binary per platform.
# Locally: goreleaser release --snapshot --clean
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/jmap-paperless /jmap-paperless
ENTRYPOINT ["/jmap-paperless"]

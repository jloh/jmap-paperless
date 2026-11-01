# Built by goreleaser, which provides the prebuilt binary per platform.
# Locally: goreleaser release --snapshot --clean
FROM gcr.io/distroless/static-debian13:nonroot
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/jmap-paperless /jmap-paperless
ENTRYPOINT ["/jmap-paperless"]

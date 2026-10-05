# The binary is built outside (make image, or the release workflow), so the
# image is the binary and nothing else: no shell, no package manager.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETARCH
COPY dist/provider-google-linux-${TARGETARCH} /provider-google
USER 65534:65534
EXPOSE 7777
ENTRYPOINT ["/provider-google"]

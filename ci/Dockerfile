ARG ALPINE=alpine:3.24.3

FROM $ALPINE

# tzdata for job time zones; sh, curl and friends for command jobs.
RUN apk --no-cache --no-progress add tzdata ca-certificates curl

# GoReleaser puts each platform's binary under linux/<arch>/ in the context.
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/kronik /

USER 65532:65532
EXPOSE 8080
ENTRYPOINT [ "/kronik" ]

FROM alpine:latest

RUN echo '@edge https://dl-cdn.alpinelinux.org/alpine/edge/community' >> /etc/apk/repositories && \
    echo '@edge-testing https://dl-cdn.alpinelinux.org/alpine/edge/testing' >> /etc/apk/repositories && \
    apk add --no-cache ttyd@edge ccze@edge-testing tmux perl

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENV HOME=/tmp
ENV ANONYMIZE_MODE=partial
ENV FILTER_WORDS="LeasewebSG"
ENV LOG_FILE=/var/log/0.log
ENV LWS_LOG_LEVEL=7

EXPOSE 7681
ENTRYPOINT ["/entrypoint.sh"]

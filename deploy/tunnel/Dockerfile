ARG ALPINE_IMAGE
FROM ${ALPINE_IMAGE} AS tunnel

RUN apk add --no-cache openssh-client
COPY entrypoint.sh /usr/local/bin/gamefleet-ssh-tunnel
RUN chmod 0555 /usr/local/bin/gamefleet-ssh-tunnel

ENTRYPOINT ["/usr/local/bin/gamefleet-ssh-tunnel"]

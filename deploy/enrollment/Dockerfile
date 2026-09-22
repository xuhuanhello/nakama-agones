# Official Python image pinned for the tested build; override only with another reviewed digest.
ARG PYTHON_IMAGE=python:3.13-slim-bookworm@sha256:2325bb286ec344af3e5898cc224b5844e2707ac6e26b1632516fd3edc84a5e26
FROM ${PYTHON_IMAGE}
ARG VCS_REF=unknown
LABEL org.opencontainers.image.source="https://github.com/xuhuanhello/nakama-agones" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.revision="${VCS_REF}"
RUN apt-get update && apt-get install -y --no-install-recommends openssh-client ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 enrollment && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin enrollment
COPY --chown=10001:10001 scripts/node_onboarding.py scripts/node_enrollment_controller.py scripts/k3s_node.py /opt/enrollment/
RUN chmod 0555 /opt/enrollment/*.py
USER 10001:10001
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
ENTRYPOINT ["python3", "-I", "/opt/enrollment/node_enrollment_controller.py", "--config", "/etc/enrollment/config.json"]

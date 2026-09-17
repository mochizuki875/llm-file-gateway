FROM mcr.microsoft.com/devcontainers/go:1.27-bookworm

ENV LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    TZ=UTC

RUN rm -f /etc/apt/sources.list.d/yarn.list \
    && apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        python3 \
        python3-venv \
        python-is-python3 \
        fonts-crosextra-carlito \
        fonts-liberation \
        fonts-noto-cjk \
        libreoffice-calc \
        libreoffice-impress \
        libreoffice-writer \
    && rm -rf /var/lib/apt/lists/*

COPY .devcontainer/fonts.conf /etc/fonts/local.conf
COPY .devcontainer/fonts/ /usr/local/share/fonts/custom/

RUN fc-cache -f
LABEL maintainer="Target Brands, Inc. TTS-CFC-OpenSource@target.com"

# Build stage
FROM ubuntu:22.04 AS builder
ARG DEBIAN_FRONTEND=noninteractive

# Versions
ARG YARA_VERSION=4.3.1
ARG EXIFTOOL_VERSION=12.60

# Environment variables
ENV PYTHONDONTWRITEBYTECODE=1
ENV PYTHONFAULTHANDLER=1
ENV PYTHONUNBUFFERED=1

# Disable apt clean to utilize cache
RUN rm -f /etc/apt/apt.conf.d/docker-clean && \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache

# Install build packages all at once
RUN apt-get -q update && \
    apt-get install -q -y --no-install-recommends \
    automake \
    build-essential \
    cmake \
    curl \
    gcc \
    git \
    dirmngr \
    gnupg \
    gpg \
    libglu1-mesa \
    libtool \
    make \
    swig \
    python3-dev \
    python3-pip \
    python3-wheel \
    python-is-python3 \
    pkg-config \
    supervisor \
    ncat \
    antiword \
    binwalk \
    libarchive-dev \
    libfuzzy-dev \
    libjansson-dev \
    libmagic-dev \
    libssl-dev \
    libzbar0 \
    libgl1 \
    python3-setuptools \
    redis-server \
    unrar \
    unzip \
    upx \
    jq \
    ca-certificates \
    zlib1g-dev \
    yasm \
    libgmp-dev \
    liblzma-dev \
    libpcap-dev \
    libbz2-dev \
    libgomp1

# Install Zeek (pulled into build stage)
RUN echo 'deb http://download.opensuse.org/repositories/security:/zeek/xUbuntu_22.04/ /' > /etc/apt/sources.list.d/security:zeek.list && \
    curl -fsSL https://download.opensuse.org/repositories/security:zeek/xUbuntu_22.04/Release.key | gpg --dearmor > /etc/apt/trusted.gpg.d/security_zeek.gpg && \
    apt-get -q update && \
    apt-get install -q -y --no-install-recommends zeek-lts && \
    cp /opt/zeek/bin/zeek /bin/zeek

# Install Suricata (pulled into build stage)
RUN apt-get install -q -y --no-install-recommends suricata && \
    apt-get install -q -y --no-install-recommends tesseract-ocr

# Install CPAN for JTR
RUN cpan -i Compress::Raw::Lzma

# Download and compile exiftool
WORKDIR /tmp
RUN curl -OL https://github.com/exiftool/exiftool/archive/refs/tags/${EXIFTOOL_VERSION}.tar.gz && \
    tar -zxf ${EXIFTOOL_VERSION}.tar.gz && \
    cd exiftool-${EXIFTOOL_VERSION}/ && \
    perl Makefile.PL && \
    make -s && \
    make -s install

# Download and move binary for 7z
RUN curl -OL https://7-zip.org/a/7z2409-linux-x64.tar.xz && \
    tar -xf 7z2409-linux-x64.tar.xz && \
    cp 7zz /usr/local/bin/

# Install YARA
RUN curl -OL https://github.com/VirusTotal/yara/archive/v${YARA_VERSION}.tar.gz && \
    tar -zxf v${YARA_VERSION}.tar.gz && \
    cd yara-${YARA_VERSION}/ && \
    ./bootstrap.sh && \
    ./configure --with-crypto --enable-magic --enable-cuckoo && \
    make -s && make -s install && make -s check

# Install yara-python
RUN curl -OL https://github.com/VirusTotal/yara-python/archive/v${YARA_VERSION}.tar.gz && \
    tar -zxf v${YARA_VERSION}.tar.gz && \
    cd yara-python-${YARA_VERSION}/ && \
    python3 setup.py build --dynamic-linking && \
    python3 setup.py install

# Install JTR
WORKDIR /
RUN mkdir jtr && cd jtr && git init && \
    git remote add origin https://github.com/openwall/john.git && \
    git fetch --depth 1 origin b5c10480f56ff1b5d76c6cbdaf9c817582ee2228 && \
    git reset --hard FETCH_HEAD && \
    rm -rf /jtr/.git && \
    cd /jtr/src && \
    ./configure && \
    make -s clean && \
    make -sj4 && \
    make install && \
    cp -Tr /jtr/run/ /jtr && rm -rf /jtr/run && \
    chmod -R 777 /jtr

# Install Poetry and project dependencies
RUN python3 -m pip install -U pip setuptools && \
    python3 -m pip install poetry

# Set up Strelka work directory
WORKDIR /strelka/

# Copy only dependency files first to leverage caching
COPY pyproject.toml poetry.lock ./

# Install dependencies
RUN poetry config virtualenvs.create false && \
    poetry install --only main

# Copy application source
COPY ./src/python/ /strelka/
COPY ./configs/python/backend/ /etc/strelka/
COPY configs/python/backend/suricata/suricata.yaml /etc/suricata/suricata.yaml

# Install Strelka
RUN cd /strelka/ && \
    python3 setup.py -q build && \
    python3 setup.py -q install

# Final stage
FROM ubuntu:22.04
ARG DEBIAN_FRONTEND=noninteractive

ENV PYTHONDONTWRITEBYTECODE=1
ENV PYTHONFAULTHANDLER=1
ENV PYTHONUNBUFFERED=1
ENV PATH=$PATH:/opt/zeek/bin

ARG USERNAME=strelka
ARG USER_UID=1001
ARG USER_GID=$USER_UID

# Create user
RUN groupadd --gid $USER_GID $USERNAME && \
    useradd --uid $USER_UID --gid $USER_GID --create-home --shell /bin/bash $USERNAME

# Install only runtime dependencies
RUN apt-get update -q && \
    apt-get install -q -y --no-install-recommends \
    python3 \
    python3-setuptools \
    python-is-python3 \
    redis-server \
    supervisor \
    ncat \
    antiword \
    binwalk \
    libarchive13 \
    libfuzzy2 \
    libjansson4 \
    libmagic1 \
    libssl3 \
    libzbar0 \
    libgl1 \
    suricata \
    tesseract-ocr \
    unrar \
    unzip \
    upx \
    jq \
    libgmp10 \
    libgomp1 \
    liblzma5 \
    libpcap0.8 \
    libbz2-1.0 \
    perl && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Copy only necessary binaries and libraries from builder
COPY --from=builder /bin/zeek /bin/zeek
COPY --from=builder /usr/local/bin/7zz /usr/local/bin/
COPY --from=builder /usr/local/lib/libyara*.so* /usr/local/lib/
COPY --from=builder /usr/local/bin/exiftool /usr/local/bin/
COPY --from=builder /jtr/ /jtr/
COPY --from=builder /etc/strelka/ /etc/strelka/
COPY --from=builder /etc/suricata/suricata.yaml /etc/suricata/suricata.yaml

# Copy Python packages and executables
COPY --from=builder /usr/local/lib/python3.10/ /usr/local/lib/python3.10/
COPY --from=builder /usr/lib/python3/dist-packages/ /usr/lib/python3/dist-packages/
COPY --from=builder /usr/local/bin/strelka* /usr/local/bin/

# Create log directory with proper permissions
RUN mkdir -p /var/log/strelka/ && \
    chgrp -R 0 /var/log/strelka/ && \
    chmod -R g=u /var/log/strelka/ && \
    # Update library cache
    ldconfig

# Expose important directories as volumes
VOLUME ["/etc/strelka", "/var/log/strelka"]

USER $USERNAME

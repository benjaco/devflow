ARG POSTGRES_MAJOR
FROM postgres:${POSTGRES_MAJOR}-bookworm

ARG POSTGRES_MAJOR

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        "postgresql-${POSTGRES_MAJOR}-postgis-3" \
        "postgresql-${POSTGRES_MAJOR}-postgis-3-scripts" \
    && rm -rf /var/lib/apt/lists/*

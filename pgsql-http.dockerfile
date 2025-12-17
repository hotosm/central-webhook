# Postgres v14
FROM postgres:14 AS pg-14

RUN apt-get update \
    && apt-get install -y postgresql-14-http \
    && rm -rf /var/lib/apt/lists/*

# Postgres v15
FROM postgres:15 AS pg-15

RUN apt-get update \
    && apt-get install -y postgresql-15-http \
    && rm -rf /var/lib/apt/lists/*

# Postgres v16
FROM postgres:16 AS pg-16

RUN apt-get update \
    && apt-get install -y postgresql-16-http \
    && rm -rf /var/lib/apt/lists/*

# Postgres v17
FROM postgres:17 AS pg-17

RUN apt-get update \
    && apt-get install -y postgresql-17-http \
    && rm -rf /var/lib/apt/lists/*

# Postgres v18
FROM postgres:18 AS pg-18

RUN apt-get update \
    && apt-get install -y postgresql-18-http \
    && rm -rf /var/lib/apt/lists/*

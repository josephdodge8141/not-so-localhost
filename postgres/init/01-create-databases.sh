#!/bin/sh
set -e

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" \
    -v "todo_pw=$TODO_DB_PASSWORD" \
    -v "keycloak_pw=$KEYCLOAK_DB_PASSWORD" \
    -v "registry_pw=$REGISTRY_DB_PASSWORD" <<-EOSQL
    CREATE USER todo WITH PASSWORD :'todo_pw';
    CREATE DATABASE todo OWNER todo;

    CREATE USER keycloak WITH PASSWORD :'keycloak_pw';
    CREATE DATABASE keycloak OWNER keycloak;

    CREATE USER registry WITH PASSWORD :'registry_pw';
    CREATE DATABASE registry OWNER registry;
EOSQL

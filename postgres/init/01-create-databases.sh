#!/bin/sh
set -e

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" \
	-v "keycloak_pw=$KEYCLOAK_DB_PASSWORD" <<-EOSQL
		    CREATE USER keycloak WITH PASSWORD :'keycloak_pw';
		    CREATE DATABASE keycloak OWNER keycloak;
	EOSQL

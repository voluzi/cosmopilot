#!/bin/sh
set -eu
# Disposable software token only; the fixture PVC is deleted with its test namespace.
mkdir /token/tokens
printf 'directories.tokendir = /token/tokens\nobjectstore.backend = file\nlog.level = ERROR\n' > /token/softhsm2.conf
export SOFTHSM2_CONF=/token/softhsm2.conf
softhsm2-util --init-token --free --label validator-token --so-pin 12345678 --pin 123456
pkcs11-tool --module /usr/lib/softhsm/libsofthsm2.so --token-label validator-token \
  --login --pin 123456 --keypairgen --key-type EC:edwards25519 --label consensus --id 01 --sensitive
printf '123456\n' > /token/pin
cosmosigner pubkey --backend pkcs11 --pkcs11-module /usr/lib/softhsm/libsofthsm2.so \
  --pkcs11-token-label validator-token --pkcs11-key-label consensus \
  --pkcs11-pin-file /token/pin --pkcs11-binding-file /token/inspection-binding.json
rm /token/pin

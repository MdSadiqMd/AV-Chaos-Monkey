package utils

import (
	cryptorand "crypto/rand"
	"encoding/hex"
)

func GenerateRandomBytes(length int) []byte {
	bytes := make([]byte, length)
	cryptorand.Read(bytes)
	return bytes
}

func GenerateIceCredentials(ufragLength, passwordLength int) (string, string) {
	ufrag := make([]byte, ufragLength)
	password := make([]byte, passwordLength)
	cryptorand.Read(ufrag)
	cryptorand.Read(password)
	return hex.EncodeToString(ufrag), hex.EncodeToString(password)
}

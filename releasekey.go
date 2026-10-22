package main

// releasePublicKey er den offentlige ed25519-nøgle (base64), som releases skal være signeret med.
// Lav nøgleparret én gang med «go run . keygen», og sæt den offentlige del ind her.
// Er den tom, kan serveren ikke tjekke releases og opdaterer sig derfor ikke selv.
var releasePublicKey = ""

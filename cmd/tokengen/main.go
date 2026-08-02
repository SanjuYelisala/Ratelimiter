package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
)

func main() {

	secret := os.Getenv("JWT_SECRET")

	//Read the JWT secret from the environment.
	if secret == "" {
		log.Fatal("JWT_SECRET environment variable is not set")

	}

	//Read the client ID from the --subject flag.

	subject := flag.String("subject", "", "client ID")

	flag.Parse()

	if *subject == "" {
		log.Fatal("--subject is required")
	}

	// Create the JWT claims.
	claims := jwt.MapClaims{
		"sub": *subject,
		"exp": time.Now().Add(24 * time.Hour).Unix(),
	}

	//Create a JWT object.
	token := jwt.NewWithClaims(
		jwt.SigningMethodHS256,
		claims,
	)

	// Sign the JWT.
	tokenString, err := token.SignedString([]byte(secret))
	if err != nil {
		log.Fatalf("failed to sign token: %v", err)
	}

	fmt.Println(tokenString)

}

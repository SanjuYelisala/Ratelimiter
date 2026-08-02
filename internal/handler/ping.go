package handler

import (
	"log"
	"net/http"
)

func Ping(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(response, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	response.Header().Set("Content-Type", "text/plain;charset=utf-8")
	_, err := response.Write([]byte("pong"))
	if err != nil {
		log.Printf("Failed to write response: %v", err)
	}
}

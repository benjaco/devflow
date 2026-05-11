package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("BACKEND_PORT")
	if port == "" {
		port = os.Getenv("PORT")
	}
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_ = r
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                 true,
			"service":            "go-next-backend-debug",
			"databaseConfigured": os.Getenv("DATABASE_URL") != "",
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_ = r
		_, _ = w.Write([]byte("go-next backend\n"))
	})

	log.Printf("backend listening on 127.0.0.1:%s", port)
	if err := http.ListenAndServe("127.0.0.1:"+port, mux); err != nil {
		log.Fatal(err)
	}
}

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Nickname string `json:"nickname"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func dsn() string {
	return "root:root@tcp(localhost:3306)/gotick?parseTime=True"
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func main() {
	ctx := context.Background()

	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis unavilable: %v", err)
	}
	log.Printf("redis ok")

	db, err := sql.Open("mysql", dsn())
	if err != nil {
		log.Fatalf("mysql open: %v", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("mysql unavilable: %v", err)
	}
	log.Printf("mysql ok")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req registerRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, "hash failed", http.StatusInternalServerError)
			return
		}

		res, err := db.ExecContext(r.Context(),
			"INSERT INTO users (email, password_hash, nickname) VALUES (?, ?, ?)",
			req.Email, string(hash), req.Nickname)
		if err != nil {
			var mysqlErr *mysql.MySQLError
			if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
				http.Error(w, "email already registered", http.StatusConflict)
				return
			}
			log.Printf("insert error: %v", err)
			http.Error(w, "insert failed", http.StatusInternalServerError)
			return
		}

		id, _ := res.LastInsertId()
		log.Printf("created user id=%d email=%s", id, req.Email)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req loginRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		var (
			id   int64
			hash string
		)
		err := db.QueryRowContext(r.Context(),
			"SELECT id, password_hash FROM users WHERE email = ?", req.Email).
			Scan(&id, &hash)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		if err != nil {
			log.Printf("query error: %v", err)
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}

		token, err := newToken()
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		if err := rdb.Set(r.Context(), "session:"+token, id, 24*time.Hour).Err(); err != nil {
			log.Printf("redis set error: %v", err)
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})

		log.Printf("login ok: user=%d", id)
	})

	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := rdb.Del(r.Context(), "session:"+token).Err(); err != nil {
			log.Printf("redis del error: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		uid, err := rdb.Get(r.Context(), "session:"+token).Int64()
		if err == redis.Nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			log.Printf("redis get error: %v", err)
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		var email, nickname string
		if err := db.QueryRowContext(r.Context(),
			"SELECT email, nickname FROM users WHERE id = ?", uid).
			Scan(&email, &nickname); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":       uid,
			"email":    email,
			"nickname": nickname,
		})
	})

	addr := ":8080"
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

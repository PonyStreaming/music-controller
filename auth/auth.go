package auth

import (
	"crypto/subtle"
	"net/http"
)

type authedHandler struct {
	password string
	realm    string
	handler  http.Handler
}

func (ah *authedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("password")), []byte(ah.password)) != 1 {
		http.Error(w, "Unauthorized.", http.StatusUnauthorized)
		return
	}

	ah.handler.ServeHTTP(w, r)
}

func Basic(handler http.Handler, password, realm string) http.Handler {
	return &authedHandler{
		password: password,
		realm:    realm,
		handler:  handler,
	}
}

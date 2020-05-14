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
	_, pass, ok := r.BasicAuth()
	if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(ah.password)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="`+ah.realm+`"`)
		w.WriteHeader(401)
		_, _ = w.Write([]byte("Unauthorised.\n"))
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

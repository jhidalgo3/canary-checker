package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func errorResonse(w http.ResponseWriter, err error, code int) {
	e, _ := json.Marshal(map[string]string{"error": err.Error()})
	fmt.Fprint(w, string(e))
	w.WriteHeader(code)
}

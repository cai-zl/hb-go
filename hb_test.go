package hb_test

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"hb"
)

func get(t *testing.T, h gin.HandlerFunc) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.GET("/", h)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v (%s)", err, w.Body.String())
	}
	return w, body
}

func TestWrapSuccessShape(t *testing.T) {
	w, body := get(t, hb.Wrap(func(c *gin.Context) (any, error) {
		return map[string]any{"a": 1}, nil
	}))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body["code"] != float64(0) || body["message"] != "ok" || body["data"].(map[string]any)["a"] != float64(1) {
		t.Fatalf("body = %v", body)
	}
}

func TestWrapNilOmitsData(t *testing.T) {
	_, body := get(t, hb.Wrap(func(c *gin.Context) (any, error) { return nil, nil }))
	if _, ok := body["data"]; ok {
		t.Fatalf("data should be omitted, got %v", body)
	}
}

func TestWrapBizError(t *testing.T) {
	_, body := get(t, hb.Wrap(func(c *gin.Context) (any, error) {
		return nil, hb.Err(400, "no such user")
	}))
	if body["code"] != float64(400) || body["message"] != "no such user" {
		t.Fatalf("body = %v", body)
	}
	if _, ok := body["data"]; ok {
		t.Fatalf("error envelope should omit data: %v", body)
	}
}

func TestErrZeroCodeIs500(t *testing.T) {
	_, body := get(t, hb.Wrap(func(c *gin.Context) (any, error) { return nil, hb.Err(0, "boom") }))
	if body["code"] != float64(500) || body["message"] != "boom" {
		t.Fatalf("body = %v", body)
	}
}

func TestWrapBareErrorMasked(t *testing.T) {
	_, body := get(t, hb.Wrap(func(c *gin.Context) (any, error) {
		return nil, errors.New("pg: connection refused")
	}))
	if body["code"] != float64(500) || body["message"] != "internal error" {
		t.Fatalf("body = %v", body)
	}
}

func TestWrapSelfWritten(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.GET("/", hb.Wrap(func(c *gin.Context) (any, error) {
		c.Data(200, "text/plain", []byte("raw"))
		return nil, nil
	}))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Body.String() != "raw" || w.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("self-written response overwritten: %s", w.Body.String())
	}
}

func TestErrf(t *testing.T) {
	var err error = hb.Errf(500, "migration: bad file name %q", "x.sql")
	if err.Error() != "500 migration: bad file name \"x.sql\"" {
		t.Fatalf("err = %q", err.Error())
	}
}

func TestPageValue(t *testing.T) {
	w, _ := get(t, hb.Wrap(func(c *gin.Context) (any, error) {
		return hb.Page([]int{1, 2}, 42, 2, 20), nil
	}))
	if want := `{"code":0,"message":"ok","data":{"list":[1,2],"total":42,"page":2,"pageSize":20}}`; w.Body.String() != want {
		t.Fatalf("body = %s, want %s", w.Body.String(), want)
	}
}

func TestWrite(t *testing.T) {
	w, body := get(t, func(c *gin.Context) {
		hb.Write(c, 401, hb.Envelope{Code: 401, Message: "missing token"})
	})
	if w.Code != 401 || body["code"] != float64(401) || body["message"] != "missing token" {
		t.Fatalf("status = %d body = %v", w.Code, body)
	}
}

func TestResponseParse(t *testing.T) {
	var r hb.Response[string]
	in := `{"code":0,"message":"ok","data":"hi"}`
	if err := json.Unmarshal([]byte(in), &r); err != nil {
		t.Fatal(err)
	}
	if r.Code != 0 || r.Message != "ok" || *r.Data != "hi" {
		t.Fatalf("r = %+v", r)
	}
	var bad hb.Response[string]
	if err := json.Unmarshal([]byte(`{"code":404,"message":"nf"}`), &bad); err != nil || bad.Data != nil {
		t.Fatalf("bad = %+v err = %v", bad, err)
	}
}

func TestPageDataParse(t *testing.T) {
	var p hb.PageData[string]
	in := `{"list":["a"],"total":9,"page":1,"pageSize":10}`
	if err := json.Unmarshal([]byte(in), &p); err != nil {
		t.Fatal(err)
	}
	if p.Total != 9 || p.PageSize != 10 || len(p.List) != 1 || p.List[0] != "a" {
		t.Fatalf("p = %+v", p)
	}
}

func TestErrorAs(t *testing.T) {
	err := hb.Err(401, "unauthorized")
	if err.Error() != "401 unauthorized" {
		t.Fatalf("err = %q", err.Error())
	}
	var e *hb.Error
	if !errors.As(err, &e) || e.Code != 401 || e.Message != "unauthorized" {
		t.Fatalf("errors.As failed: %v", err)
	}
	if hb.Internal(nil) != nil {
		t.Fatal("Internal(nil) should be nil")
	}
	masked := hb.Internal(errors.New("pg: refused"))
	if masked.Error() != "500 internal error: pg: refused" {
		t.Fatalf("masked = %q", masked.Error())
	}
	if !errors.As(masked, &e) || e.Unwrap() == nil || e.Unwrap().Error() != "pg: refused" {
		t.Fatal("origin lost")
	}
}

func TestFrameworkErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := hb.New()
	e.GET("/hi", func(c *gin.Context) { c.JSON(200, hb.Envelope{Code: 0, Message: "ok"}) })

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/nope", nil))
	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != float64(404) || body["message"] != "Not Found" {
		t.Fatalf("body = %v", body)
	}

	w = httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("POST", "/hi", nil))
	if w.Code != 405 {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != float64(405) || body["message"] != "Method Not Allowed" {
		t.Fatalf("body = %v", body)
	}
}

func TestRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(hb.Recovery())
	e.GET("/", func(c *gin.Context) { panic("explode") })
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v (%s)", err, w.Body.String())
	}
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if body["code"] != float64(500) || body["message"] != "internal error" {
		t.Fatalf("body = %v", body)
	}
}

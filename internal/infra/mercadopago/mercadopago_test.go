package mercadopago

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ProblemaTheu/oficina-billing/internal/application/cobranca"
	"github.com/ProblemaTheu/oficina-billing/internal/domain/orcamento"
)

// cliente + stub reais conversando por HTTP: valida o adaptador e o stub juntos.
func clienteComStub(t *testing.T) (*Cliente, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(NovoStub("http://checkout", "", "segredo"))
	t.Cleanup(srv.Close)
	return Novo(srv.URL, "token"), srv
}

func pagar(t *testing.T, srv *httptest.Server, order, resultado string) int {
	t.Helper()
	corpo, _ := json.Marshal(map[string]string{"order_id": order, "resultado": resultado})
	resp, err := http.Post(srv.URL+"/_stub/pagar", "application/json", bytes.NewReader(corpo))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func novaCobranca() cobranca.NovaCobranca {
	return cobranca.NovaCobranca{OrcamentoID: uuid.New(), NumeroOS: "OS-1", Valor: 5780, EmailPagador: "m@x.com"}
}

func TestCicloCompleto_CobrarPagarEstornar(t *testing.T) {
	c, srv := clienteComStub(t)
	ctx := context.Background()
	n := novaCobranca()

	cob, err := c.CriarCobranca(ctx, n)
	if err != nil || cob.OrderID == "" || cob.CheckoutURL != "http://checkout/checkout/"+cob.OrderID {
		t.Fatalf("CriarCobranca = %+v, %v", cob, err)
	}
	// Mesma idempotency key (orcamento_id): mesma order.
	if deNovo, _ := c.CriarCobranca(ctx, n); deNovo.OrderID != cob.OrderID {
		t.Error("criar duas vezes deve devolver a mesma order")
	}
	if e, _ := c.Consultar(ctx, cob.OrderID); e.Status != "created" {
		t.Errorf("antes de pagar: %+v", e)
	}

	pagar(t, srv, cob.OrderID, "processed")
	e, err := c.Consultar(ctx, cob.OrderID)
	if err != nil || e.Status != "processed" || e.StatusDetail != "accredited" || e.Pago != orcamento.Centavos(5780) {
		t.Fatalf("depois de pagar: %+v %v", e, err)
	}

	ref, err := c.Estornar(ctx, cob.OrderID, "chave-1")
	if err != nil || ref == "" {
		t.Fatalf("Estornar: %q %v", ref, err)
	}
	if mesmo, err := c.Estornar(ctx, cob.OrderID, "chave-1"); err != nil || mesmo != ref {
		t.Errorf("mesma chave = mesmo estorno: %q %v", mesmo, err)
	}
	if _, err := c.Estornar(ctx, cob.OrderID, "chave-2"); !errors.Is(err, cobranca.ErrJaEstornado) {
		t.Errorf("chave nova em order estornada → 409 → ErrJaEstornado: %v", err)
	}
}

// Comportamento do sandbox: recusa deixa a order pagável; cancelada não aceita.
func TestRecusaECancelamento(t *testing.T) {
	c, srv := clienteComStub(t)
	ctx := context.Background()
	cob, _ := c.CriarCobranca(ctx, novaCobranca())

	pagar(t, srv, cob.OrderID, "failed")
	e, _ := c.Consultar(ctx, cob.OrderID)
	if e.Status != "action_required" || e.StatusDetail != "rejected_by_issuer" {
		t.Fatalf("tentativa recusada: %+v (o detalhe vem da última tentativa)", e)
	}
	if err := c.Cancelar(ctx, cob.OrderID); err != nil {
		t.Fatal(err)
	}
	if status := pagar(t, srv, cob.OrderID, "processed"); status != http.StatusConflict {
		t.Errorf("order cancelada aceitou pagamento: %d", status)
	}
	if err := c.Cancelar(ctx, cob.OrderID); err != nil {
		t.Errorf("cancelar de novo é idempotente: %v", err)
	}
	if _, err := c.Estornar(ctx, cob.OrderID, "k"); err == nil {
		t.Error("estornar order não paga deve falhar")
	}
}

func TestCancelarOrderPagaFalha(t *testing.T) {
	c, srv := clienteComStub(t)
	cob, _ := c.CriarCobranca(context.Background(), novaCobranca())
	pagar(t, srv, cob.OrderID, "processed")
	if err := c.Cancelar(context.Background(), cob.OrderID); err == nil {
		t.Error("order já paga não pode ser cancelada")
	}
}

func TestOrderInexistenteEValidacao(t *testing.T) {
	c, srv := clienteComStub(t)
	if _, err := c.Consultar(context.Background(), "NAOEXISTE"); err == nil {
		t.Error("order inexistente deve falhar")
	}
	if status := pagar(t, srv, "NAOEXISTE", "processed"); status != http.StatusNotFound {
		t.Errorf("pagar inexistente: %d", status)
	}
	sem := novaCobranca()
	sem.Valor, sem.EmailPagador = 0, ""
	if _, err := c.CriarCobranca(context.Background(), sem); err != nil {
		t.Errorf("valor zero (\"0.00\") é aceito: %v", err)
	}
	resp, _ := http.Post(srv.URL+"/v1/orders", "application/json", bytes.NewReader([]byte(`{}`)))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("sem total_amount: %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/rota/que/nao/existe")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("rota inexistente: %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/checkout/NAOEXISTE")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("checkout de order inexistente: %d", resp.StatusCode)
	}
	cob, _ := c.CriarCobranca(context.Background(), novaCobranca())
	resp, _ = http.Get(srv.URL + "/checkout/" + cob.OrderID)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("página de checkout: %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/health/live")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health: %d", resp.StatusCode)
	}
}

func TestRetry_SoEmRedeE5xx(t *testing.T) {
	var chamadas atomic.Int32
	cincoXX := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if chamadas.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Header.Get("X-Idempotency-Key") == "" || r.Header.Get("Authorization") != "Bearer token" {
			t.Error("cabeçalhos ausentes")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"ORD1","checkout_url":"http://x"}`)
	}))
	defer cincoXX.Close()
	c := Novo(cincoXX.URL, "token")
	c.pausa = time.Millisecond
	if cob, err := c.CriarCobranca(context.Background(), novaCobranca()); err != nil || cob.OrderID != "ORD1" || chamadas.Load() != 3 {
		t.Errorf("5xx deve ser repetido: %+v %v (%d chamadas)", cob, err, chamadas.Load())
	}

	chamadas.Store(0)
	quatroXX := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chamadas.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer quatroXX.Close()
	if _, err := Novo(quatroXX.URL, "t").CriarCobranca(context.Background(), novaCobranca()); err == nil || chamadas.Load() != 1 {
		t.Errorf("4xx não pode ser repetido: %v (%d chamadas)", err, chamadas.Load())
	}

	semResposta := Novo("http://127.0.0.1:1", "t")
	semResposta.pausa = time.Millisecond
	if _, err := semResposta.Consultar(context.Background(), "X"); err == nil {
		t.Error("rede fora deve falhar após as tentativas")
	}

	vazio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer vazio.Close()
	if _, err := Novo(vazio.URL, "t").CriarCobranca(context.Background(), novaCobranca()); err == nil {
		t.Error("order sem id deve falhar")
	}
	if ref, err := Novo(vazio.URL, "t").Estornar(context.Background(), "X", "k"); err != nil || ref != "" {
		t.Errorf("estorno sem refund no corpo: %q %v", ref, err)
	}
}

func TestRetry_RespeitaCancelamentoDoContexto(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer srv.Close()
	c := Novo(srv.URL, "t")
	c.pausa = time.Hour
	ctx, cancelar := context.WithCancel(context.Background())
	cancelar()
	if _, err := c.Consultar(ctx, "X"); err == nil {
		t.Error("contexto cancelado deve interromper o retry")
	}
}

func TestAssinatura(t *testing.T) {
	agora := time.Unix(1794350643, 0)
	ts := strconv.FormatInt(agora.Unix(), 10)
	v1 := Assinar("segredo", "req-1", "ORDTST01ABC", ts)
	cabecalho := "ts=" + ts + ",v1=" + v1

	if !AssinaturaValida("segredo", cabecalho, "req-1", "ORDTST01ABC", agora) {
		t.Fatal("assinatura válida rejeitada")
	}
	if !AssinaturaValida("segredo", " ts="+ts+" , v1="+v1, "req-1", "ordtst01abc", agora) {
		t.Error("data.id é comparado em minúsculas e espaços no cabeçalho são tolerados")
	}
	casos := map[string]func() bool{
		"segredo errado":     func() bool { return AssinaturaValida("outro", cabecalho, "req-1", "ORDTST01ABC", agora) },
		"request-id trocado": func() bool { return AssinaturaValida("segredo", cabecalho, "req-2", "ORDTST01ABC", agora) },
		"data.id trocado":    func() bool { return AssinaturaValida("segredo", cabecalho, "req-1", "ORDTST01XYZ", agora) },
		"fora da janela": func() bool {
			return AssinaturaValida("segredo", cabecalho, "req-1", "ORDTST01ABC", agora.Add(6*time.Minute))
		},
		"sem v1":          func() bool { return AssinaturaValida("segredo", "ts="+ts, "req-1", "ORDTST01ABC", agora) },
		"ts não numérico": func() bool { return AssinaturaValida("segredo", "ts=abc,v1="+v1, "req-1", "ORDTST01ABC", agora) },
		"sem data.id":     func() bool { return AssinaturaValida("segredo", cabecalho, "req-1", "", agora) },
		"sem segredo":     func() bool { return AssinaturaValida("", cabecalho, "req-1", "ORDTST01ABC", agora) },
	}
	for nome, f := range casos {
		if f() {
			t.Errorf("%s: aceita indevidamente", nome)
		}
	}
}

// O stub notifica o webhook do Billing com x-signature válido.
func TestStub_NotificaWebhookAssinado(t *testing.T) {
	recebido := make(chan *http.Request, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recebido <- r
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	stub := httptest.NewServer(NovoStub("http://checkout", webhook.URL, "segredo-webhook"))
	defer stub.Close()
	c := Novo(stub.URL, "t")
	cob, _ := c.CriarCobranca(context.Background(), novaCobranca())

	pagar(t, stub, cob.OrderID, "processed")
	select {
	case r := <-recebido:
		if r.URL.Query().Get("type") != "order" || r.URL.Query().Get("data.id") != cob.OrderID {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		if !AssinaturaValida("segredo-webhook", r.Header.Get("x-signature"), r.Header.Get("x-request-id"), cob.OrderID, time.Now()) {
			t.Error("o stub deve assinar como o Mercado Pago")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook não chegou")
	}
}

func TestStub_WebhookForaDoArNaoQuebraOPagamento(t *testing.T) {
	stub := httptest.NewServer(NovoStub("http://checkout", "http://127.0.0.1:1/webhook", "s"))
	defer stub.Close()
	cob, _ := Novo(stub.URL, "t").CriarCobranca(context.Background(), novaCobranca())
	if status := pagar(t, stub, cob.OrderID, "processed"); status != http.StatusOK {
		t.Errorf("pagar com webhook fora do ar: %d", status)
	}
}

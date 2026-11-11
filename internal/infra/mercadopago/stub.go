package mercadopago

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Stub é um Mercado Pago em memória com a mesma API de Orders (F4-2.6): roda no
// compose e no BDD, sem internet nem navegador. NUNCA é implantado em
// produção — o overlay de prod não o referencia e MP_BASE_URL vem do ConfigMap.
//
// Comportamento copiado do sandbox (10/11): pagamento recusado deixa a order em
// action_required/waiting_retry (aceita nova tentativa); order cancelada não
// aceita pagamento; segundo estorno → 409 order_already_refunded; mesma
// idempotency key → mesmo estorno.
type Stub struct {
	mu       sync.Mutex
	orders   map[string]*orderStub
	porChave map[string]string // idempotency key → order id (criação)
	base     string            // URL pública do stub (checkout_url)
	webhook  string            // URL do webhook do Billing ("" = não notifica)
	segredo  string
	http     *http.Client
	agora    func() time.Time
}

type orderStub struct {
	ID                string         `json:"id"`
	Status            string         `json:"status"`
	StatusDetail      string         `json:"status_detail"`
	TotalAmount       string         `json:"total_amount"`
	TotalPaidAmount   string         `json:"total_paid_amount,omitempty"`
	ExternalReference string         `json:"external_reference"`
	CheckoutURL       string         `json:"checkout_url"`
	Transactions      transacoesStub `json:"transactions"`
	refundPorChave    map[string]string
}

type transacoesStub struct {
	Payments []pagamentoStub `json:"payments"`
	Refunds  []refundStub    `json:"refunds,omitempty"`
}

type pagamentoStub struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	StatusDetail string `json:"status_detail"`
}

type refundStub struct {
	ID     string `json:"id"`
	Amount string `json:"amount"`
	Status string `json:"status"`
}

// NovoStub cria o stub. webhook vazio = não envia notificações.
func NovoStub(base, webhook, segredo string) *Stub {
	return &Stub{orders: map[string]*orderStub{}, porChave: map[string]string{}, base: strings.TrimRight(base, "/"),
		webhook: webhook, segredo: segredo, http: &http.Client{Timeout: 3 * time.Second}, agora: time.Now}
}

// ServeHTTP roteia a API de Orders e as rotas de controle (/_stub/*).
func (s *Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	partes := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.URL.Path == "/health/live":
		responder(w, http.StatusOK, map[string]string{"status": "UP"})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/orders":
		s.criar(w, r)
	case r.Method == http.MethodGet && len(partes) == 3 && partes[1] == "orders":
		s.comOrder(w, partes[2], func(o *orderStub) (int, any) { return http.StatusOK, o })
	case r.Method == http.MethodPost && len(partes) == 4 && partes[3] == "cancel":
		s.comOrder(w, partes[2], s.cancelar)
	case r.Method == http.MethodPost && len(partes) == 4 && partes[3] == "refund":
		s.comOrder(w, partes[2], func(o *orderStub) (int, any) { return s.estornar(o, r.Header.Get("X-Idempotency-Key")) })
	case r.Method == http.MethodPost && r.URL.Path == "/_stub/pagar":
		s.pagar(w, r)
	case r.Method == http.MethodGet && len(partes) == 2 && partes[0] == "checkout":
		s.paginaCheckout(w, partes[1])
	default:
		responder(w, http.StatusNotFound, map[string]string{"message": "rota inexistente no stub"})
	}
}

func responder(w http.ResponseWriter, status int, corpo any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(corpo)
}

func (s *Stub) criar(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TotalAmount       string `json:"total_amount"`
		ExternalReference string `json:"external_reference"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TotalAmount == "" {
		responder(w, http.StatusBadRequest, map[string]string{"message": "total_amount obrigatório"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.porChave[r.Header.Get("X-Idempotency-Key")]; ok && id != "" {
		responder(w, http.StatusCreated, s.orders[id])
		return
	}
	id := "ORDTST" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:26]
	o := &orderStub{ID: id, Status: "created", StatusDetail: "created", TotalAmount: req.TotalAmount,
		ExternalReference: req.ExternalReference, CheckoutURL: s.base + "/checkout/" + id, refundPorChave: map[string]string{}}
	s.orders[id] = o
	if k := r.Header.Get("X-Idempotency-Key"); k != "" {
		s.porChave[k] = id
	}
	responder(w, http.StatusCreated, o)
}

func (s *Stub) comOrder(w http.ResponseWriter, id string, fn func(*orderStub) (int, any)) {
	s.mu.Lock()
	o, ok := s.orders[id]
	if !ok {
		s.mu.Unlock()
		responder(w, http.StatusNotFound, map[string]string{"message": "order not found"})
		return
	}
	status, corpo := fn(o)
	s.mu.Unlock()
	responder(w, status, corpo)
}

func (s *Stub) cancelar(o *orderStub) (int, any) {
	switch o.Status {
	case "created", "action_required":
		o.Status, o.StatusDetail = "canceled", "canceled"
		return http.StatusOK, o
	case "canceled":
		return http.StatusOK, o
	default:
		return http.StatusConflict, map[string]string{"code": "cannot_cancel_order", "message": "order " + o.Status}
	}
}

func (s *Stub) estornar(o *orderStub, chave string) (int, any) {
	if _, repetido := o.refundPorChave[chave]; repetido && chave != "" {
		return http.StatusCreated, o // mesma idempotency key → mesmo estorno
	}
	if o.Status == "refunded" {
		return http.StatusConflict, map[string]any{"errors": []map[string]string{{"code": "order_already_refunded", "message": "the order is already refunded."}}}
	}
	if o.Status != "processed" {
		return http.StatusConflict, map[string]string{"code": "cannot_refund_order", "message": "order " + o.Status}
	}
	ref := refundStub{ID: "REF" + strings.ToUpper(uuid.NewString()[:8]), Amount: o.TotalAmount, Status: "processed"}
	o.Status, o.StatusDetail = "refunded", "refunded"
	o.Transactions.Refunds = append(o.Transactions.Refunds, ref)
	o.refundPorChave[chave] = ref.ID
	return http.StatusCreated, o
}

// pagar simula o comprador no checkout: "processed" aprova; "failed" recusa a
// tentativa e deixa a order aguardando nova tentativa (como o sandbox).
func (s *Stub) pagar(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderID   string `json:"order_id"`
		Resultado string `json:"resultado"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	o, ok := s.orders[req.OrderID]
	if !ok {
		s.mu.Unlock()
		responder(w, http.StatusNotFound, map[string]string{"message": "order not found"})
		return
	}
	if o.Status != "created" && o.Status != "action_required" {
		s.mu.Unlock()
		responder(w, http.StatusConflict, map[string]string{"message": "O que você queria pagar não está disponível"})
		return
	}
	pag := pagamentoStub{ID: "PAY" + strings.ToUpper(uuid.NewString()[:8])}
	if req.Resultado == "failed" {
		pag.Status, pag.StatusDetail = "failed", "rejected_by_issuer"
		o.Status, o.StatusDetail = "action_required", "waiting_retry"
	} else {
		pag.Status, pag.StatusDetail = "processed", "accredited"
		o.Status, o.StatusDetail, o.TotalPaidAmount = "processed", "accredited", o.TotalAmount
	}
	o.Transactions.Payments = append(o.Transactions.Payments, pag)
	copia := *o
	s.mu.Unlock()
	s.notificar(copia.ID)
	responder(w, http.StatusOK, copia)
}

// notificar envia o webhook assinado (o Mercado Pago real não notifica orders
// de teste; aqui notificamos para exercitar o handler — o reconciliador do
// Billing confirma de qualquer jeito).
func (s *Stub) notificar(orderID string) {
	if s.webhook == "" {
		return
	}
	requestID, ts := uuid.NewString(), strconv.FormatInt(s.agora().Unix(), 10)
	corpo, _ := json.Marshal(map[string]any{"action": "order.processed", "type": "order", "data": map[string]string{"id": orderID}})
	req, err := http.NewRequest(http.MethodPost, s.webhook+"?data.id="+orderID+"&type=order", bytes.NewReader(corpo))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-request-id", requestID)
	req.Header.Set("x-signature", "ts="+ts+",v1="+Assinar(s.segredo, requestID, orderID, ts))
	resp, err := s.http.Do(req)
	if err != nil {
		slog.Warn("stub: webhook não entregue", "error", err)
		return
	}
	_ = resp.Body.Close()
}

// paginaCheckout explica como pagar. Só imprime o id guardado no stub — nunca
// o que veio na URL (sem refletir entrada do usuário na resposta).
func (s *Stub) paginaCheckout(w http.ResponseWriter, id string) {
	s.mu.Lock()
	o, ok := s.orders[id]
	s.mu.Unlock()
	if !ok {
		responder(w, http.StatusNotFound, map[string]string{"message": "order not found"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "Checkout do stub do Mercado Pago (order %s, R$ %s).\nPara pagar: POST /_stub/pagar com order_id e resultado processed|failed\n", o.ID, o.TotalAmount)
}

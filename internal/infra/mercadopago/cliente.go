// Package mercadopago fala com o Mercado Pago pela API de Orders (Checkout Pro).
//
// API de Orders, e não Preferences: validado no sandbox em 10/11 — Preferences
// cobra, mas POST /v1/payments/{id}/refunds responde 401 para pagamentos de
// conta de teste; com /v1/orders cobrança, consulta, cancelamento e estorno
// funcionam (docs/planejamentos/fase-4/execucao.md do oficina-app).
package mercadopago

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ProblemaTheu/oficina-billing/internal/application/cobranca"
	"github.com/ProblemaTheu/oficina-billing/internal/domain/orcamento"
)

// Cliente implementa cobranca.Gateway. Sem SDK: são 4 chamadas, e o SDK Go
// oficial é pouco mantido.
type Cliente struct {
	base       string
	token      string
	http       *http.Client
	tentativas int
	pausa      time.Duration
}

var _ cobranca.Gateway = (*Cliente)(nil)

// Novo cria o cliente. base = https://api.mercadopago.com (prod) ou o stub.
func Novo(base, token string) *Cliente {
	return &Cliente{base: base, token: token, http: &http.Client{Timeout: 5 * time.Second}, tentativas: 3, pausa: 300 * time.Millisecond}
}

// erroHTTP é uma resposta de erro do Mercado Pago.
type erroHTTP struct {
	status int
	corpo  string
}

func (e erroHTTP) Error() string {
	return fmt.Sprintf("mercado pago respondeu %d: %s", e.status, e.corpo)
}

// chamar faz a requisição com retry só em erro de rede ou 5xx (nunca em 4xx:
// repetir um pedido inválido não o conserta). A idempotency key torna o retry
// seguro em POST.
func (c *Cliente) chamar(ctx context.Context, metodo, caminho, chave string, corpo, destino any) error {
	var payload []byte
	if corpo != nil {
		var err error
		if payload, err = json.Marshal(corpo); err != nil {
			return err
		}
	}
	var ultimo error
	for t := 0; t < c.tentativas; t++ {
		if t > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.pausa * time.Duration(t)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, metodo, c.base+caminho, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")
		if chave != "" {
			req.Header.Set("X-Idempotency-Key", chave)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			ultimo = err
			continue
		}
		dados, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 {
			ultimo = erroHTTP{resp.StatusCode, string(dados)}
			continue
		}
		if resp.StatusCode >= 400 {
			return erroHTTP{resp.StatusCode, string(dados)}
		}
		if destino != nil {
			return json.Unmarshal(dados, destino)
		}
		return nil
	}
	return fmt.Errorf("mercado pago indisponível após %d tentativas: %w", c.tentativas, ultimo)
}

// CriarCobranca cria uma order online (Checkout Pro). Valores em string decimal.
func (c *Cliente) CriarCobranca(ctx context.Context, n cobranca.NovaCobranca) (cobranca.Cobranca, error) {
	corpo := map[string]any{
		"type":               "online",
		"processing_mode":    "manual", // exigido pelo Checkout Pro na API de Orders
		"total_amount":       n.Valor.String(),
		"external_reference": n.OrcamentoID.String(),
		"items": []map[string]any{{
			"title": "Orçamento " + n.NumeroOS, "unit_price": n.Valor.String(), "quantity": 1,
		}},
	}
	if n.EmailPagador != "" {
		corpo["payer"] = map[string]string{"email": n.EmailPagador}
	}
	var resp struct {
		ID          string `json:"id"`
		CheckoutURL string `json:"checkout_url"`
	}
	if err := c.chamar(ctx, http.MethodPost, "/v1/orders", n.OrcamentoID.String(), corpo, &resp); err != nil {
		return cobranca.Cobranca{}, err
	}
	if resp.ID == "" || resp.CheckoutURL == "" {
		return cobranca.Cobranca{}, errors.New("mercado pago: order sem id ou checkout_url")
	}
	return cobranca.Cobranca{OrderID: resp.ID, CheckoutURL: resp.CheckoutURL}, nil
}

// Consultar lê a order. Em action_required, o detalhe útil é o da última
// tentativa de pagamento (ex.: rejected_by_issuer).
func (c *Cliente) Consultar(ctx context.Context, orderID string) (cobranca.EstadoDaCobranca, error) {
	var resp struct {
		Status          string `json:"status"`
		StatusDetail    string `json:"status_detail"`
		TotalPaidAmount string `json:"total_paid_amount"`
		Transactions    struct {
			Payments []struct {
				Status       string `json:"status"`
				StatusDetail string `json:"status_detail"`
			} `json:"payments"`
		} `json:"transactions"`
	}
	if err := c.chamar(ctx, http.MethodGet, "/v1/orders/"+orderID, "", nil, &resp); err != nil {
		return cobranca.EstadoDaCobranca{}, err
	}
	e := cobranca.EstadoDaCobranca{Status: resp.Status, StatusDetail: resp.StatusDetail}
	if n := len(resp.Transactions.Payments); n > 0 && resp.Status != "processed" {
		if d := resp.Transactions.Payments[n-1].StatusDetail; d != "" {
			e.StatusDetail = d
		}
	}
	if resp.TotalPaidAmount != "" {
		if pago, err := orcamento.ParseCentavos(resp.TotalPaidAmount); err == nil {
			e.Pago = pago
		}
	}
	return e, nil
}

// Cancelar fecha a order: o link de pagamento passa a recusar novas tentativas.
func (c *Cliente) Cancelar(ctx context.Context, orderID string) error {
	return c.chamar(ctx, http.MethodPost, "/v1/orders/"+orderID+"/cancel", "cancel-"+orderID, nil, nil)
}

// Estornar devolve o valor total. Mesma chave → mesmo estorno (idempotente);
// order já estornada (409 order_already_refunded) → ErrJaEstornado.
func (c *Cliente) Estornar(ctx context.Context, orderID, chave string) (string, error) {
	var resp struct {
		Transactions struct {
			Refunds []struct {
				ID string `json:"id"`
			} `json:"refunds"`
		} `json:"transactions"`
	}
	err := c.chamar(ctx, http.MethodPost, "/v1/orders/"+orderID+"/refund", chave, nil, &resp)
	var e erroHTTP
	if errors.As(err, &e) && e.status == http.StatusConflict {
		return "", cobranca.ErrJaEstornado
	}
	if err != nil {
		return "", err
	}
	if len(resp.Transactions.Refunds) == 0 {
		return "", nil
	}
	return resp.Transactions.Refunds[0].ID, nil
}

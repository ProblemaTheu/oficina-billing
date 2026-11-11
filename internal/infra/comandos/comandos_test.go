package comandos

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ProblemaTheu/oficina-billing/internal/application/cobranca"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/mensageria"
)

type ucFake struct {
	chamadas []string
	cmd      cobranca.Comando
	p        cobranca.PayloadGerarOrcamento
	motivo   string
}

func (u *ucFake) GerarOrcamento(_ context.Context, c cobranca.Comando, p cobranca.PayloadGerarOrcamento) error {
	u.chamadas, u.cmd, u.p = append(u.chamadas, "gerar"), c, p
	return nil
}
func (u *ucFake) EstornarPagamento(_ context.Context, c cobranca.Comando) error {
	u.chamadas, u.cmd = append(u.chamadas, "estornar"), c
	return nil
}
func (u *ucFake) CancelarOrcamento(_ context.Context, c cobranca.Comando, motivo string) error {
	u.chamadas, u.cmd, u.motivo = append(u.chamadas, "cancelar"), c, motivo
	return nil
}

func env(t *testing.T, tipo string, payload any) mensageria.Envelope {
	t.Helper()
	e, err := mensageria.Novo(tipo, uuid.New(), uuid.New(), "", mensageria.OrigemOS, payload, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestHandlers(t *testing.T) {
	uc := &ucFake{}
	hs := Handlers(uc)

	g := env(t, cobranca.TipoGerarOrcamento, map[string]any{"numero_os": "OS-1", "itens": []map[string]any{{"nome": "Filtro", "qtd": 1, "preco": "10.00"}}})
	_ = hs[cobranca.TipoGerarOrcamento](context.Background(), g)
	if uc.cmd.MessageID != g.MessageID || uc.p.NumeroOS != "OS-1" || len(uc.p.Itens) != 1 {
		t.Errorf("GerarOrcamento repassado errado: %+v %+v", uc.cmd, uc.p)
	}

	e := env(t, cobranca.TipoEstornarPagamento, map[string]string{"motivo": "x"})
	_ = hs[cobranca.TipoEstornarPagamento](context.Background(), e)
	c := env(t, cobranca.TipoCancelarOrcamento, map[string]string{"motivo": "compensacao_T5"})
	_ = hs[cobranca.TipoCancelarOrcamento](context.Background(), c)
	if uc.motivo != "compensacao_T5" || uc.cmd.OsID != c.OsID {
		t.Errorf("CancelarOrcamento: motivo %q", uc.motivo)
	}
	if len(uc.chamadas) != 3 || uc.chamadas[1] != "estornar" {
		t.Errorf("chamadas = %v", uc.chamadas)
	}
}

func TestGerarOrcamento_PayloadInvalidoSegueVazio(t *testing.T) {
	uc := &ucFake{}
	e := env(t, cobranca.TipoGerarOrcamento, nil)
	e.Payload = json.RawMessage(`{"itens":"nao-e-lista"}`)
	if err := Handlers(uc)[cobranca.TipoGerarOrcamento](context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(uc.p.Itens) != 0 || len(uc.chamadas) != 1 {
		t.Error("payload inválido chega vazio ao caso de uso (que responde OrcamentoFalhou)")
	}
}

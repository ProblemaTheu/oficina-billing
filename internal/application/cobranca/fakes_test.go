package cobranca

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/ProblemaTheu/oficina-billing/internal/domain/orcamento"
)

var errFalha = errors.New("falhou")

type uowFake struct{ execucoes int }

func (u *uowFake) Executar(ctx context.Context, fn func(context.Context) error) error {
	u.execucoes++
	return fn(ctx)
}

type inboxFake struct {
	vistas map[uuid.UUID]bool
	err    error
}

func (i *inboxFake) Marcar(_ context.Context, id uuid.UUID, _ string) (bool, error) {
	if i.err != nil {
		return false, i.err
	}
	if i.vistas[id] {
		return false, nil
	}
	i.vistas[id] = true
	return true, nil
}

type orcamentosFake struct {
	porID              map[uuid.UUID]orcamento.Orcamento
	errCriar, errMudar error
	errBuscar          error
}

func (o *orcamentosFake) Criar(_ context.Context, x orcamento.Orcamento) error {
	if o.errCriar != nil {
		return o.errCriar
	}
	o.porID[x.ID] = x
	return nil
}
func (o *orcamentosFake) Buscar(_ context.Context, id uuid.UUID) (orcamento.Orcamento, error) {
	if o.errBuscar != nil {
		return orcamento.Orcamento{}, o.errBuscar
	}
	x, ok := o.porID[id]
	if !ok {
		return x, ErrNaoEncontrado
	}
	return x, nil
}
func (o *orcamentosFake) BuscarPorOS(_ context.Context, osID uuid.UUID) (orcamento.Orcamento, error) {
	if o.errBuscar != nil {
		return orcamento.Orcamento{}, o.errBuscar
	}
	for _, x := range o.porID {
		if x.OsID == osID {
			return x, nil
		}
	}
	return orcamento.Orcamento{}, ErrNaoEncontrado
}
func (o *orcamentosFake) MudarStatus(_ context.Context, x orcamento.Orcamento, anterior orcamento.Status) error {
	if o.errMudar != nil {
		return o.errMudar
	}
	if o.porID[x.ID].Status != anterior {
		return ErrConflito
	}
	o.porID[x.ID] = x
	return nil
}

type pagamentosFake struct {
	porID              map[uuid.UUID]orcamento.Pagamento
	errCriar, errMudar error
	errBuscar          error
}

func (p *pagamentosFake) Criar(_ context.Context, x orcamento.Pagamento) error {
	if p.errCriar != nil {
		return p.errCriar
	}
	p.porID[x.ID] = x
	return nil
}
func (p *pagamentosFake) BuscarPorOrcamento(_ context.Context, id uuid.UUID) (orcamento.Pagamento, error) {
	if p.errBuscar != nil {
		return orcamento.Pagamento{}, p.errBuscar
	}
	for _, x := range p.porID {
		if x.OrcamentoID == id {
			return x, nil
		}
	}
	return orcamento.Pagamento{}, ErrNaoEncontrado
}
func (p *pagamentosFake) BuscarPorOrder(_ context.Context, order string) (orcamento.Pagamento, error) {
	for _, x := range p.porID {
		if x.MPOrderID == order {
			return x, nil
		}
	}
	return orcamento.Pagamento{}, ErrNaoEncontrado
}
func (p *pagamentosFake) MudarStatus(_ context.Context, x orcamento.Pagamento, anterior orcamento.StatusPagamento) error {
	if p.errMudar != nil {
		return p.errMudar
	}
	if p.porID[x.ID].Status != anterior {
		return ErrConflito
	}
	p.porID[x.ID] = x
	return nil
}
func (p *pagamentosFake) Pendentes(_ context.Context, _ int) ([]orcamento.Pagamento, error) {
	if p.errBuscar != nil {
		return nil, p.errBuscar
	}
	var out []orcamento.Pagamento
	for _, x := range p.porID {
		if x.Status == orcamento.PagamentoPendente {
			out = append(out, x)
		}
	}
	return out, nil
}

type respostasFake struct {
	saidas []Saida
	err    error
}

func (r *respostasFake) Responder(_ context.Context, s Saida) error {
	if r.err != nil {
		return r.err
	}
	r.saidas = append(r.saidas, s)
	return nil
}

func (r *respostasFake) tipos() []string {
	var out []string
	for _, s := range r.saidas {
		out = append(out, s.Tipo)
	}
	return out
}

type gatewayFake struct {
	cobranca                      Cobranca
	estado                        EstadoDaCobranca
	errCriar, errConsultar        error
	errCancelar, errEstornar      error
	refund                        string
	criadas, canceladas, estornos []string
	chaves                        []string
}

func (g *gatewayFake) CriarCobranca(_ context.Context, n NovaCobranca) (Cobranca, error) {
	if g.errCriar != nil {
		return Cobranca{}, g.errCriar
	}
	g.criadas = append(g.criadas, n.Valor.String())
	return g.cobranca, nil
}
func (g *gatewayFake) Consultar(context.Context, string) (EstadoDaCobranca, error) {
	return g.estado, g.errConsultar
}
func (g *gatewayFake) Cancelar(_ context.Context, id string) error {
	g.canceladas = append(g.canceladas, id)
	return g.errCancelar
}
func (g *gatewayFake) Estornar(_ context.Context, id, chave string) (string, error) {
	g.estornos, g.chaves = append(g.estornos, id), append(g.chaves, chave)
	return g.refund, g.errEstornar
}

type notificadorFake struct {
	para, assunto, corpo string
	err                  error
}

func (n *notificadorFake) Enviar(_ context.Context, para, assunto, corpo string) error {
	n.para, n.assunto, n.corpo = para, assunto, corpo
	return n.err
}

type cenario struct {
	s     *Servico
	orc   *orcamentosFake
	pag   *pagamentosFake
	resp  *respostasFake
	gw    *gatewayFake
	inbox *inboxFake
	notif *notificadorFake
}

var relogio = time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC)

func novoCenario() cenario {
	c := cenario{
		orc: &orcamentosFake{porID: map[uuid.UUID]orcamento.Orcamento{}}, pag: &pagamentosFake{porID: map[uuid.UUID]orcamento.Pagamento{}},
		resp: &respostasFake{}, gw: &gatewayFake{cobranca: Cobranca{OrderID: "ORDTST1", CheckoutURL: "http://mp/checkout/ORDTST1"}, refund: "REF1"},
		inbox: &inboxFake{vistas: map[uuid.UUID]bool{}}, notif: &notificadorFake{},
	}
	c.s = Novo(Dependencias{UoW: &uowFake{}, Inbox: c.inbox, Orcamentos: c.orc, Pagamentos: c.pag, Respostas: c.resp,
		Gateway: c.gw, Notificar: c.notif, URLPublica: "http://billing"})
	c.s.agora = func() time.Time { return relogio }
	return c
}

// comOrcamento grava um orçamento no status dado (e, se pago, uma cobrança).
func (c cenario) comOrcamento(status orcamento.Status) orcamento.Orcamento {
	o, _ := orcamento.Novo(uuid.New(), uuid.New(), "OS-1", orcamento.Cliente{ID: uuid.New(), Nome: "Maria", Email: "m@x.com"}, "diag",
		[]orcamento.Item{{Nome: "Filtro", Quantidade: 2, PrecoUnitario: 8990}}, relogio)
	o.Status = status
	c.orc.porID[o.ID] = o
	return o
}

func (c cenario) comPagamento(o orcamento.Orcamento, status orcamento.StatusPagamento) orcamento.Pagamento {
	p := orcamento.Pagamento{ID: uuid.New(), OrcamentoID: o.ID, MPOrderID: "ORD-" + o.ID.String()[:6], CheckoutURL: "http://mp/x",
		Status: status, Valor: o.Total}
	c.pag.porID[p.ID] = p
	return p
}

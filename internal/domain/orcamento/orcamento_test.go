package orcamento

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParseCentavos(t *testing.T) {
	validos := map[string]Centavos{"89.90": 8990, "89.9": 8990, "89": 8900, "0.05": 5, " 1200.00 ": 120000, "0": 0}
	for s, esperado := range validos {
		if c, err := ParseCentavos(s); err != nil || c != esperado {
			t.Errorf("ParseCentavos(%q) = %d, %v; esperava %d", s, c, err, esperado)
		}
	}
	for _, s := range []string{"", "abc", "-1.00", "+1", "1.", "1.234", "1,50", "1.a"} {
		if _, err := ParseCentavos(s); !errors.Is(err, ErrValorInvalido) {
			t.Errorf("ParseCentavos(%q) deveria falhar, veio %v", s, err)
		}
	}
}

func TestCentavos_String(t *testing.T) {
	for c, s := range map[Centavos]string{8990: "89.90", 5: "0.05", 0: "0.00", 120000: "1200.00"} {
		if c.String() != s {
			t.Errorf("%d.String() = %q", c, c.String())
		}
	}
}

// 0.1 + 0.2 em float daria 0.30000000000000004; em centavos não existe esse erro.
func TestCentavos_SemErroDeArredondamento(t *testing.T) {
	a, _ := ParseCentavos("0.10")
	b, _ := ParseCentavos("0.20")
	if (a + b).String() != "0.30" {
		t.Errorf("0.10 + 0.20 = %s", (a + b).String())
	}
}

var agora = time.Date(2026, 11, 11, 17, 0, 0, 0, time.UTC)

func novo(t *testing.T) Orcamento {
	t.Helper()
	o, err := Novo(uuid.New(), uuid.New(), "OS-1", Cliente{ID: uuid.New(), Nome: "Maria", Email: "m@x.com"}, "diag",
		[]Item{{Tipo: "peca", Nome: "Filtro", Quantidade: 2, PrecoUnitario: 8990}, {Tipo: "servico", Nome: "Troca", Quantidade: 1, PrecoUnitario: 5000}}, agora)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestNovo_SomaOTotal(t *testing.T) {
	o := novo(t)
	if o.Total != 2*8990+5000 || o.Status != Gerado || o.ID == uuid.Nil {
		t.Errorf("orçamento = %+v", o)
	}
}

func TestNovo_Validacoes(t *testing.T) {
	cli := Cliente{}
	if _, err := Novo(uuid.Nil, uuid.New(), "", cli, "", []Item{{Nome: "x", Quantidade: 1}}, agora); err == nil {
		t.Error("os_id vazio")
	}
	if _, err := Novo(uuid.New(), uuid.New(), "", cli, "", nil, agora); !errors.Is(err, ErrSemItens) {
		t.Errorf("sem itens: %v", err)
	}
	for _, i := range []Item{{Nome: "x", Quantidade: 0}, {Nome: "x", Quantidade: 1, PrecoUnitario: -1}, {Nome: " ", Quantidade: 1}} {
		if _, err := Novo(uuid.New(), uuid.New(), "", cli, "", []Item{i}, agora); !errors.Is(err, ErrItemInvalido) {
			t.Errorf("item %+v: %v", i, err)
		}
	}
}

func TestTransicoes(t *testing.T) {
	o := novo(t)
	if err := o.Aprovar(agora); err != nil || o.Status != Aprovado {
		t.Fatalf("aprovar: %v", err)
	}
	if err := o.Aprovar(agora); !errors.Is(err, ErrEstadoInvalido) {
		t.Errorf("aprovar duas vezes: %v", err)
	}
	if err := o.Recusar("caro", agora); !errors.Is(err, ErrEstadoInvalido) {
		t.Errorf("recusar aprovado: %v", err)
	}
	if err := o.Cancelar("compensacao", agora); err != nil || o.Status != Cancelado || o.Motivo != "compensacao" {
		t.Fatalf("cancelar aprovado: %v %+v", err, o)
	}
	if err := o.Cancelar("de novo", agora); err != nil || o.Motivo != "compensacao" {
		t.Errorf("cancelar é idempotente: %v %q", err, o.Motivo)
	}

	r := novo(t)
	if err := r.Recusar("caro demais", agora); err != nil || r.Status != Recusado || r.Motivo != "caro demais" {
		t.Errorf("recusar: %v %+v", err, r)
	}
	if err := r.Cancelar("compensacao", agora); err != nil || r.Status != Cancelado {
		t.Errorf("cancelar recusado: %v", err)
	}
}

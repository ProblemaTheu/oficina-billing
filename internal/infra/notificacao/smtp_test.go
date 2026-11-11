package notificacao

import (
	"context"
	"errors"
	"net/smtp"
	"strings"
	"testing"
)

func TestSMTP_MontaAMensagem(t *testing.T) {
	var (
		addr, de string
		para     []string
		msg      []byte
	)
	s := NovoSMTP("mailpit", "1025", "billing@oficina.local", "", "")
	s.enviar = func(a string, _ smtp.Auth, f string, to []string, m []byte) error {
		addr, de, para, msg = a, f, to, m
		return nil
	}
	if err := s.Enviar(context.Background(), "maria@exemplo.com", "Orçamento da OS OS-1", "Total: R$ 57.80"); err != nil {
		t.Fatal(err)
	}
	corpo := string(msg)
	if addr != "mailpit:1025" || de != "billing@oficina.local" || para[0] != "maria@exemplo.com" {
		t.Errorf("envelope SMTP: %s %s %v", addr, de, para)
	}
	if !strings.Contains(corpo, "To: maria@exemplo.com") || !strings.Contains(corpo, "=?utf-8?q?") || !strings.HasSuffix(corpo, "Total: R$ 57.80") {
		t.Errorf("mensagem = %q", corpo)
	}
}

func TestSMTP_RejeitaInjecaoDeCabecalho(t *testing.T) {
	s := NovoSMTP("h", "25", "de@x", "usuario", "senha")
	s.enviar = func(string, smtp.Auth, string, []string, []byte) error { t.Error("não deveria enviar"); return nil }
	if err := s.Enviar(context.Background(), "a@x\r\nBcc: b@x", "assunto", "c"); err == nil {
		t.Error("quebra de linha no destinatário deve ser rejeitada")
	}
	if s.auth == nil {
		t.Error("com usuário, usa PLAIN auth")
	}
}

func TestSMTP_PropagaErro(t *testing.T) {
	s := NovoSMTP("h", "25", "de@x", "", "")
	falha := errors.New("smtp fora")
	s.enviar = func(string, smtp.Auth, string, []string, []byte) error { return falha }
	if err := s.Enviar(context.Background(), "a@x", "s", "c"); !errors.Is(err, falha) {
		t.Errorf("erro: %v", err)
	}
}

func TestLog(t *testing.T) {
	if err := (Log{}).Enviar(context.Background(), "a@x", "s", "c"); err != nil {
		t.Error(err)
	}
}

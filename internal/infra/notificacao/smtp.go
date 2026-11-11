// Package notificacao envia o orçamento ao cliente por e-mail (SMTP — Mailpit
// no compose; um provedor real em produção).
package notificacao

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"net/smtp"
	"strings"
)

// SMTP envia mensagens de texto simples sem autenticação (Mailpit) ou com
// PLAIN auth quando usuário e senha estão configurados.
type SMTP struct {
	endereco  string
	remetente string
	auth      smtp.Auth
	enviar    func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

// NovoSMTP cria o notificador.
func NovoSMTP(host, porta, remetente, usuario, senha string) *SMTP {
	var auth smtp.Auth
	if usuario != "" {
		auth = smtp.PlainAuth("", usuario, senha, host)
	}
	return &SMTP{endereco: host + ":" + porta, remetente: remetente, auth: auth, enviar: smtp.SendMail}
}

// Enviar manda o e-mail. Cabeçalhos com quebra de linha são rejeitados
// (injeção de cabeçalho).
func (s *SMTP) Enviar(_ context.Context, para, assunto, corpo string) error {
	if strings.ContainsAny(para+assunto, "\r\n") {
		return fmt.Errorf("cabeçalho de e-mail inválido")
	}
	msg := "From: " + s.remetente + "\r\n" +
		"To: " + para + "\r\n" +
		"Subject: " + mime.QEncoding.Encode("utf-8", assunto) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" + corpo
	return s.enviar(s.endereco, s.auth, s.remetente, []string{para}, []byte(msg))
}

// Log só registra (ambiente sem SMTP).
type Log struct{}

// Enviar registra o e-mail no log.
func (Log) Enviar(ctx context.Context, para, assunto, _ string) error {
	slog.InfoContext(ctx, "e-mail (notificador de log)", "para", para, "assunto", assunto)
	return nil
}

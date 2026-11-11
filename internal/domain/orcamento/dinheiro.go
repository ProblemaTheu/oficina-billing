// Package orcamento modela o orçamento de uma OS e o pagamento dele (Fase 4).
package orcamento

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Centavos é o valor monetário do Billing — inteiro, sem float em nenhum ponto.
// O OS Service envia preços como string decimal ("89.90"), e o Mercado Pago
// recebe string decimal também: a conversão é de texto para inteiro e volta.
type Centavos int64

// ErrValorInvalido indica um valor monetário malformado.
var ErrValorInvalido = errors.New("valor monetário inválido")

// ParseCentavos lê "89.90", "89.9" ou "89" (ponto como separador decimal).
func ParseCentavos(s string) (Centavos, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return 0, fmt.Errorf("%w: %q", ErrValorInvalido, s)
	}
	inteiro, fracao, temFracao := strings.Cut(s, ".")
	if temFracao && (len(fracao) == 0 || len(fracao) > 2) {
		return 0, fmt.Errorf("%w: %q (até 2 casas decimais)", ErrValorInvalido, s)
	}
	for len(fracao) < 2 {
		fracao += "0"
	}
	reais, err := strconv.ParseInt(inteiro, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrValorInvalido, s)
	}
	cents, err := strconv.ParseInt(fracao, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrValorInvalido, s)
	}
	return Centavos(reais*100 + cents), nil
}

// String formata como decimal com 2 casas ("89.90").
func (c Centavos) String() string {
	return fmt.Sprintf("%d.%02d", int64(c)/100, int64(c)%100)
}

-- Banco próprio do Billing Service (Fase 4). Nenhum outro serviço acessa.
-- Dinheiro em centavos (bigint): sem float em nenhum ponto do Billing.

CREATE TABLE "orcamentos" (
  "id"                   uuid PRIMARY KEY,
  "os_id"                uuid NOT NULL UNIQUE,  -- um orçamento por OS (idempotência de negócio)
  "saga_id"              uuid NOT NULL,
  "numero_os"            varchar(30),
  "cliente_id"           uuid,
  "cliente_nome"         text,
  "cliente_email"        text,
  "status"               varchar(12) NOT NULL
    CONSTRAINT "chk_orcamentos_status" CHECK ("status" IN ('gerado', 'aprovado', 'recusado', 'cancelado')),
  "diagnostico"          text,
  "valor_total_centavos" bigint NOT NULL CONSTRAINT "chk_orcamentos_total" CHECK ("valor_total_centavos" >= 0),
  "motivo"               text,
  "criado_em"            timestamptz NOT NULL DEFAULT now(),
  "atualizado_em"        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE "itens_orcamento" (
  "id"                      uuid PRIMARY KEY,
  "orcamento_id"            uuid NOT NULL REFERENCES "orcamentos" ("id"),
  "ordem"                   integer NOT NULL,
  "tipo"                    varchar(10),
  "referencia"              text,
  "nome"                    text NOT NULL,
  "quantidade"              integer NOT NULL CONSTRAINT "chk_itens_quantidade" CHECK ("quantidade" > 0),
  "preco_unitario_centavos" bigint NOT NULL CONSTRAINT "chk_itens_preco" CHECK ("preco_unitario_centavos" >= 0)
);
CREATE INDEX "ix_itens_orcamento" ON "itens_orcamento" ("orcamento_id", "ordem");

-- Uma cobrança por orçamento; mp_order_id UNIQUE é a chave de idempotência do
-- webhook/reconciliação (a transição pendente → * só acontece uma vez).
CREATE TABLE "pagamentos" (
  "id"             uuid PRIMARY KEY,
  "orcamento_id"   uuid NOT NULL UNIQUE REFERENCES "orcamentos" ("id"),
  "mp_order_id"    text NOT NULL UNIQUE,
  "checkout_url"   text NOT NULL,
  "status"         varchar(12) NOT NULL
    CONSTRAINT "chk_pagamentos_status" CHECK ("status" IN ('pendente', 'aprovado', 'recusado', 'estornado', 'cancelado')),
  "valor_centavos" bigint NOT NULL,
  "status_detail"  text,
  "mp_refund_id"   text,
  "criado_em"      timestamptz NOT NULL DEFAULT now(),
  "atualizado_em"  timestamptz NOT NULL DEFAULT now()
);
-- Fila do reconciliador.
CREATE INDEX "ix_pagamentos_pendentes" ON "pagamentos" ("criado_em") WHERE "status" = 'pendente';

-- Auditoria do webhook do Mercado Pago (a idempotência é por mp_order_id).
CREATE TABLE "notificacoes_mp" (
  "x_request_id" text PRIMARY KEY,
  "tipo"         text,
  "data_id"      text,
  "recebido_em"  timestamptz NOT NULL DEFAULT now()
);

-- Outbox e inbox: mesmo DDL do OS Service.
CREATE TABLE "outbox" (
  "id"            uuid PRIMARY KEY,
  "seq"           bigint GENERATED ALWAYS AS IDENTITY,
  "destino"       varchar(30) NOT NULL,
  "tipo"          varchar(40) NOT NULL,
  "envelope"      jsonb NOT NULL,
  "trace_headers" jsonb NOT NULL DEFAULT '{}',
  "criado_em"     timestamptz NOT NULL DEFAULT now(),
  "enviado_em"    timestamptz,
  "tentativas"    integer NOT NULL DEFAULT 0,
  "falhou_em"     timestamptz
);
CREATE INDEX "ix_outbox_pendentes" ON "outbox" ("seq") WHERE "enviado_em" IS NULL AND "falhou_em" IS NULL;

CREATE TABLE "mensagens_processadas" (
  "message_id"    uuid PRIMARY KEY,
  "tipo"          varchar(40) NOT NULL,
  "processado_em" timestamptz NOT NULL DEFAULT now()
);

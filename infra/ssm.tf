# Contrato com os outros repositórios (e com o CD deste): parâmetros nomeados.
# O CD lê estes valores e gera o ConfigMap billing-ambiente; o oficina-app
# lê a URL da fila de comandos para enviar GerarOrcamento etc.

resource "aws_ssm_parameter" "fila_comandos_url" {
  name  = "/oficina/${var.ambiente}/billing/fila-comandos-url"
  type  = "String"
  value = aws_sqs_queue.comandos.url
}

resource "aws_ssm_parameter" "db_host" {
  name  = "/oficina/${var.ambiente}/billing/db/host"
  type  = "String"
  value = aws_db_instance.billing.address
}

resource "aws_ssm_parameter" "db_secret_arn" {
  name  = "/oficina/${var.ambiente}/billing/db/secret_arn"
  type  = "String"
  value = aws_secretsmanager_secret.db.arn
}

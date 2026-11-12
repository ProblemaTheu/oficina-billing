# Rede e cluster vêm do oficina-infra-k8s pela SSM — nenhum state lê o
# state de outro repositório.

data "aws_caller_identity" "atual" {}

data "aws_ssm_parameter" "vpc_id" {
  name = "/oficina/shared/vpc/id"
}

data "aws_ssm_parameter" "subnets_privadas" {
  name = "/oficina/shared/vpc/subnets_privadas"
}

data "aws_ssm_parameter" "node_sg" {
  name = "/oficina/shared/eks/node_sg_id"
}

data "aws_ssm_parameter" "cluster_name" {
  name = "/oficina/shared/eks/cluster_name"
}

locals {
  nome             = "oficina-billing"
  subnets_privadas = split(",", data.aws_ssm_parameter.subnets_privadas.value)

  # Fila de respostas do orquestrador: é do oficina-app. ARN por convenção de
  # nome para não criar dependência circular entre os states (arquitetura §7).
  fila_respostas_arn = "arn:aws:sqs:${var.regiao}:${data.aws_caller_identity.atual.account_id}:os-saga-replies.fifo"
}

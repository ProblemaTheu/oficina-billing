# Banco PRÓPRIO do Billing: instância separada do RDS do OS (oficina-infra-db).
# Nenhum outro serviço tem rota de rede nem credencial para ele.
# Mesmo desenho do oficina-infra-db (recursos diretos, sem módulo).

# `special = false`: a senha entra numa DSN; caracteres especiais exigiriam
# escape em todo consumidor.
resource "random_password" "db" {
  length  = 32
  special = false
}

resource "aws_db_subnet_group" "billing" {
  name       = local.nome
  subnet_ids = local.subnets_privadas
}

# 5432 aberto SOMENTE para os nós do EKS (os pods saem com o SG do nó).
resource "aws_security_group" "billing_db" {
  name        = "${local.nome}-rds"
  description = "PostgreSQL do Billing Service"
  vpc_id      = data.aws_ssm_parameter.vpc_id.value
}

resource "aws_vpc_security_group_ingress_rule" "nos_eks" {
  security_group_id            = aws_security_group.billing_db.id
  description                  = "Nos do EKS"
  from_port                    = 5432
  to_port                      = 5432
  ip_protocol                  = "tcp"
  referenced_security_group_id = data.aws_ssm_parameter.node_sg.value
}

resource "aws_db_instance" "billing" {
  identifier = local.nome

  engine         = "postgres"
  engine_version = var.engine_version
  instance_class = var.instance_class

  allocated_storage     = 20
  max_allocated_storage = 50
  storage_type          = "gp3"
  storage_encrypted     = true # só dá para ligar na criação

  db_name  = "billing"
  username = "billing"
  password = random_password.db.result
  port     = 5432

  db_subnet_group_name   = aws_db_subnet_group.billing.name
  vpc_security_group_ids = [aws_security_group.billing_db.id]

  multi_az            = false
  publicly_accessible = false

  backup_retention_period = 7    # dentro da cota gratuita de backup (= tamanho do banco)
  skip_final_snapshot     = true # ambiente de desafio, destruído após a entrega
  deletion_protection     = false
  apply_immediately       = true

  auto_minor_version_upgrade = true
}

# O state não é cofre: quem consome a senha (scripts/prod-secret.sh) lê daqui.
resource "aws_secretsmanager_secret" "db" {
  name                    = "oficina/${var.ambiente}/billing/db"
  recovery_window_in_days = 0 # permite recriar com o mesmo nome após destroy
}

resource "aws_secretsmanager_secret_version" "db" {
  secret_id = aws_secretsmanager_secret.db.id
  secret_string = jsonencode({
    username = aws_db_instance.billing.username
    password = random_password.db.result
    host     = aws_db_instance.billing.address
    port     = aws_db_instance.billing.port
    dbname   = aws_db_instance.billing.db_name
  })
}

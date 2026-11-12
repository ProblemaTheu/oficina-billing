# Identidade AWS do pod do Billing por EKS Pod Identity (ADR-016): o addon
# eks-pod-identity-agent já existe no cluster (oficina-infra-k8s).
#
# Menor privilégio: consumir a PRÓPRIA fila e enviar só para a fila de
# respostas do orquestrador. Sem acesso a DynamoDB, a outras filas nem a
# qualquer banco — o RDS do Billing é acessado por rede + senha, não por IAM.

resource "aws_iam_role" "pod" {
  name = "${local.nome}-pod"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "pods.eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
      Condition = {
        StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.atual.account_id }
      }
    }]
  })
}

resource "aws_iam_role_policy" "pod" {
  name = "saga-billing"
  role = aws_iam_role.pod.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "ConsumirComandos"
        Effect   = "Allow"
        Action   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes", "sqs:GetQueueUrl"]
        Resource = aws_sqs_queue.comandos.arn
      },
      {
        Sid      = "ResponderAoOrquestrador"
        Effect   = "Allow"
        Action   = ["sqs:SendMessage", "sqs:GetQueueAttributes", "sqs:GetQueueUrl"]
        Resource = local.fila_respostas_arn
      }
    ]
  })
}

resource "aws_eks_pod_identity_association" "billing" {
  cluster_name    = data.aws_ssm_parameter.cluster_name.value
  namespace       = var.namespace
  service_account = var.service_account
  role_arn        = aws_iam_role.pod.arn
}

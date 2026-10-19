resource "aws_kms_key" "runtime" {
  description             = "Miles backend SSE-KMS and encrypted DSG credentials"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  lifecycle { prevent_destroy = true }
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "EnableAccountIAMPolicies"
      Effect    = "Allow"
      Principal = { AWS = "arn:aws:iam::${var.account_id}:root" }
      Action    = "kms:*"
      Resource  = "*"
    }]
  })
}
resource "aws_kms_alias" "runtime" {
  name          = "alias/${var.name}-runtime"
  target_key_id = aws_kms_key.runtime.key_id
}
# Metadata only: no aws_secretsmanager_secret_version, bearer, key or hash in Terraform.
resource "aws_secretsmanager_secret" "runtime" {
  name                    = "${var.name}/runtime-secrets"
  description             = "Provision secret material separately; Terraform manages metadata only"
  kms_key_id              = aws_kms_key.runtime.arn
  recovery_window_in_days = 30
}
resource "aws_ssm_parameter" "config" {
  name        = "/${var.name}/bootstrap-config"
  description = "Non-secret bootstrap locations, not authoritative SystemConfig policy"
  type        = "String"
  value       = jsonencode({ region = var.region, secret_arn = aws_secretsmanager_secret.runtime.arn, schema_version = 1 })
}
resource "aws_cloudwatch_log_group" "runtime" {
  name              = "/${var.name}/runtime"
  retention_in_days = 14
}
output "kms_key_arn" { value = aws_kms_key.runtime.arn }
output "secret_arn" { value = aws_secretsmanager_secret.runtime.arn }
output "parameter_arn" { value = aws_ssm_parameter.config.arn }
output "log_group_arn" { value = aws_cloudwatch_log_group.runtime.arn }

locals {
  services = toset(["kms", "ssm", "secretsmanager", "logs", "monitoring", "sts", "elasticloadbalancing", "autoscaling"])
}
resource "aws_vpc_endpoint" "gateway" {
  for_each          = toset(["s3", "dynamodb"])
  vpc_id            = var.vpc_id
  service_name      = "com.amazonaws.${var.region}.${each.key}"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = var.route_table_ids
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = "*"
      Action    = each.key == "s3" ? ["s3:ListBucket", "s3:GetBucketLocation", "s3:ListBucketMultipartUploads", "s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:RestoreObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"] : ["dynamodb:GetItem", "dynamodb:BatchGetItem", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DeleteItem", "dynamodb:BatchWriteItem", "dynamodb:Query", "dynamodb:Scan", "dynamodb:DescribeTable", "dynamodb:ConditionCheckItem"]
      Resource  = each.key == "s3" ? ["arn:aws:s3:::${var.data_bucket_name}", "arn:aws:s3:::${var.data_bucket_name}/*"] : concat(var.table_arns, [for a in var.table_arns : "${a}/index/*"])
    }]
  })
  tags = { Name = "${var.name}-${each.key}" }
}
resource "aws_security_group" "interfaces" {
  count       = var.enable_interfaces ? 1 : 0
  name_prefix = "${var.name}-endpoints-"
  description = "PrivateLink HTTPS from Miles runtime only"
  vpc_id      = var.vpc_id
}
resource "aws_vpc_security_group_ingress_rule" "interfaces" {
  for_each                     = var.enable_interfaces ? { gateway = var.gateway_sg_id, jobs = var.jobs_sg_id } : {}
  security_group_id            = aws_security_group.interfaces[0].id
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}
resource "aws_vpc_endpoint" "interface" {
  for_each            = var.enable_interfaces ? local.services : toset([])
  vpc_id              = var.vpc_id
  service_name        = "com.amazonaws.${var.region}.${each.key}"
  vpc_endpoint_type   = "Interface"
  private_dns_enabled = true
  subnet_ids          = values(var.private_subnets)
  security_group_ids  = [aws_security_group.interfaces[0].id]
  tags                = { Name = "${var.name}-${each.key}" }
}
output "gateway_ids" { value = { for n, e in aws_vpc_endpoint.gateway : n => e.id } }
output "interface_ids" { value = { for n, e in aws_vpc_endpoint.interface : n => e.id } }

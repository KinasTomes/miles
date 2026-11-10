variable "enabled" { type = bool }
variable "name" { type = string }
variable "region" { type = string }
variable "account_id" { type = string }
variable "ami_id" { type = string }
variable "instance_type" { type = string }
variable "subnet_id" { type = string }
variable "vpc_id" { type = string }
variable "endpoint_security_group_id" { type = string }
variable "alb_security_group_id" { type = string }
variable "permissions_boundary_arn" { type = string }
variable "kms_key_arn" { type = string }
variable "data_bucket_name" { type = string }
variable "owner_ssh_public_key" {
  type    = string
  default = null
  validation {
    condition     = !var.enabled || can(regex("^ssh-ed25519 [A-Za-z0-9+/=]+( .*)?$", var.owner_ssh_public_key))
    error_message = "SSM-tunneled operator access requires an Ed25519 public key."
  }
}

locals {
  nodes = var.enabled ? toset(["edge", "client"]) : toset([])
}
data "aws_ami" "host" {
  count  = var.enabled ? 1 : 0
  owners = ["amazon"]
  filter {
    name   = "image-id"
    values = [var.ami_id]
  }
  filter {
    name   = "architecture"
    values = ["arm64"]
  }
  filter {
    name   = "name"
    values = ["al2023-ami-2023.*-kernel-*-arm64"]
  }
  filter {
    name   = "root-device-type"
    values = ["ebs"]
  }
}
resource "aws_iam_role" "node" {
  for_each             = local.nodes
  name                 = "${var.name}-acceptance-${each.key}"
  path                 = "/miles/dev/"
  permissions_boundary = var.permissions_boundary_arn
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{
    Effect = "Allow", Action = "sts:AssumeRole", Principal = { Service = "ec2.amazonaws.com" }
  }] })
}
resource "aws_iam_role_policy" "node" {
  for_each = local.nodes
  name     = "private-acceptance-bootstrap"
  role     = aws_iam_role.node[each.key].id
  policy = jsonencode({ Version = "2012-10-17", Statement = [
    { Effect = "Allow", Action = "ssm:UpdateInstanceInformation", Resource = "arn:aws:ec2:${var.region}:${var.account_id}:instance/*", Condition = { StringEquals = { "aws:RequestedRegion" = var.region } } },
    { Effect = "Allow", Action = ["ssmmessages:CreateControlChannel", "ssmmessages:CreateDataChannel", "ssmmessages:OpenControlChannel", "ssmmessages:OpenDataChannel", "logs:DescribeLogGroups"], Resource = "*", Condition = { StringEquals = { "aws:RequestedRegion" = var.region } } },
    { Effect = "Allow", Action = "s3:GetObjectVersion", Resource = "arn:aws:s3:::${var.data_bucket_name}/acceptance-assets/${each.key}/*" },
    { Effect = "Allow", Action = "kms:Decrypt", Resource = var.kms_key_arn, Condition = { StringEquals = { "kms:ViaService" = "s3.${var.region}.amazonaws.com" } } }
  ] })
}
resource "aws_iam_instance_profile" "node" {
  for_each = local.nodes
  name     = "${var.name}-acceptance-${each.key}"
  path     = "/miles/dev/"
  role     = aws_iam_role.node[each.key].name
}
resource "aws_security_group" "node" {
  for_each    = local.nodes
  name_prefix = "${var.name}-acceptance-${each.key}-"
  description = "Private disposable acceptance node managed through SSM"
  vpc_id      = var.vpc_id
}
resource "aws_vpc_security_group_egress_rule" "https" {
  for_each          = local.nodes
  security_group_id = aws_security_group.node[each.key].id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}
resource "aws_vpc_security_group_egress_rule" "ntp" {
  for_each          = local.nodes
  security_group_id = aws_security_group.node[each.key].id
  cidr_ipv4         = "169.254.169.123/32"
  ip_protocol       = "udp"
  from_port         = 123
  to_port           = 123
}
resource "aws_vpc_security_group_ingress_rule" "endpoints" {
  for_each                     = local.nodes
  security_group_id            = var.endpoint_security_group_id
  referenced_security_group_id = aws_security_group.node[each.key].id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}
resource "aws_vpc_security_group_ingress_rule" "client_edge" {
  count                        = var.enabled ? 1 : 0
  security_group_id            = aws_security_group.node["edge"].id
  referenced_security_group_id = aws_security_group.node["client"].id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}
resource "aws_vpc_security_group_ingress_rule" "client_alb" {
  count                        = var.enabled ? 1 : 0
  security_group_id            = var.alb_security_group_id
  referenced_security_group_id = aws_security_group.node["client"].id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}
resource "aws_instance" "node" {
  for_each                    = local.nodes
  ami                         = var.ami_id
  instance_type               = var.instance_type
  subnet_id                   = var.subnet_id
  associate_public_ip_address = false
  vpc_security_group_ids      = [aws_security_group.node[each.key].id]
  iam_instance_profile        = aws_iam_instance_profile.node[each.key].name
  user_data = "#cloud-config\n${yamlencode({
    users  = ["default", { name = "ec2-user", ssh_authorized_keys = [var.owner_ssh_public_key] }]
    runcmd = [["systemctl", "enable", "--now", "amazon-ssm-agent", "chronyd"], ["cat", "/etc/ssh/ssh_host_ed25519_key.pub"]]
  })}"
  user_data_replace_on_change = true
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }
  credit_specification { cpu_credits = "standard" }
  root_block_device {
    volume_size           = 20
    volume_type           = "gp3"
    encrypted             = true
    kms_key_id            = var.kms_key_arn
    delete_on_termination = true
    tags                  = { Project = "Miles", Environment = "dev" }
  }
  tags       = { Name = "${var.name}-acceptance-${each.key}", Purpose = "EdgeAcceptance" }
  lifecycle {
    precondition {
      condition     = data.aws_ami.host[0].id == var.ami_id
      error_message = "The pinned host image must be an Amazon Linux ARM64 EBS image."
    }
  }
  depends_on = [aws_iam_role_policy.node, aws_vpc_security_group_ingress_rule.endpoints]
}
output "nodes" {
  value = { for name, node in aws_instance.node : name => {
    instance_id = node.id
    private_ip  = node.private_ip
    volume_id   = node.root_block_device[0].volume_id
    role_arn    = aws_iam_role.node[name].arn
  } }
}

locals {
  zones     = { for i, z in var.zones : z => i }
  nat_zones = var.egress_mode == "nat" ? (var.nat_per_az ? local.zones : { (var.zones[0]) = 0 }) : {}
}
resource "aws_vpc" "this" {
  cidr_block           = var.cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = var.name }
}
resource "aws_internet_gateway" "this" { vpc_id = aws_vpc.this.id }
resource "aws_subnet" "public" {
  for_each                = local.zones
  vpc_id                  = aws_vpc.this.id
  availability_zone       = each.key
  cidr_block              = cidrsubnet(var.cidr, 8, each.value)
  map_public_ip_on_launch = false
  tags                    = { Name = "${var.name}-public-${each.key}" }
}
resource "aws_subnet" "private" {
  for_each                = local.zones
  vpc_id                  = aws_vpc.this.id
  availability_zone       = each.key
  cidr_block              = cidrsubnet(var.cidr, 8, 100 + each.value)
  map_public_ip_on_launch = false
  tags                    = { Name = "${var.name}-private-${each.key}" }
}
resource "aws_route_table" "public" { vpc_id = aws_vpc.this.id }
resource "aws_route" "internet" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.this.id
}
resource "aws_route_table_association" "public" {
  for_each       = local.zones
  subnet_id      = aws_subnet.public[each.key].id
  route_table_id = aws_route_table.public.id
}
resource "aws_route_table" "private" {
  for_each = local.zones
  vpc_id   = aws_vpc.this.id
  tags     = { Name = "${var.name}-private-${each.key}" }
}
resource "aws_route_table_association" "private" {
  for_each       = local.zones
  subnet_id      = aws_subnet.private[each.key].id
  route_table_id = aws_route_table.private[each.key].id
}
resource "aws_eip" "nat" {
  for_each = local.nat_zones
  domain   = "vpc"
}
resource "aws_nat_gateway" "this" {
  for_each      = local.nat_zones
  allocation_id = aws_eip.nat[each.key].id
  subnet_id     = aws_subnet.public[each.key].id
  depends_on    = [aws_internet_gateway.this, aws_route.internet, aws_route_table_association.public]
}
resource "aws_route" "nat" {
  for_each               = var.egress_mode == "nat" ? local.zones : {}
  route_table_id         = aws_route_table.private[each.key].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.this[var.nat_per_az ? each.key : var.zones[0]].id
}
resource "aws_security_group" "this" {
  for_each    = toset(["alb", "gateway", "jobs", "monitor", "admin"])
  name_prefix = "${var.name}-${each.key}-"
  description = "Miles ${each.key}; explicit rules only"
  vpc_id      = aws_vpc.this.id
  tags        = { Name = "${var.name}-${each.key}" }
}
locals {
  inbound = {
    api     = { from = "alb", to = "gateway", port = 8080 }
    health  = { from = "alb", to = "gateway", port = 9100 }
    metrics = { from = "monitor", to = "gateway", port = 9100 }
    admin   = { from = "admin", to = "gateway", port = 9000 }
  }
}
resource "aws_vpc_security_group_ingress_rule" "internal" {
  for_each                     = local.inbound
  security_group_id            = aws_security_group.this[each.value.to].id
  referenced_security_group_id = aws_security_group.this[each.value.from].id
  ip_protocol                  = "tcp"
  from_port                    = each.value.port
  to_port                      = each.value.port
}
resource "aws_vpc_security_group_egress_rule" "internal" {
  for_each                     = local.inbound
  security_group_id            = aws_security_group.this[each.value.from].id
  referenced_security_group_id = aws_security_group.this[each.value.to].id
  ip_protocol                  = "tcp"
  from_port                    = each.value.port
  to_port                      = each.value.port
}
resource "aws_vpc_security_group_ingress_rule" "https" {
  for_each          = var.client_cidrs
  security_group_id = aws_security_group.this["alb"].id
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}
resource "aws_vpc_security_group_ingress_rule" "ssh" {
  for_each          = var.admin_ssh_cidrs
  security_group_id = aws_security_group.this["admin"].id
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 22
  to_port           = 22
}
# Gateway endpoint prefix lists route S3/DDB; interface mode stays within VPC.
resource "aws_vpc_security_group_egress_rule" "https" {
  for_each          = toset(["gateway", "jobs"])
  security_group_id = aws_security_group.this[each.key].id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

output "vpc_id" { value = aws_vpc.this.id }
output "public_subnets" { value = { for z, s in aws_subnet.public : z => s.id } }
output "private_subnets" { value = { for z, s in aws_subnet.private : z => s.id } }
output "private_route_table_ids" { value = [for r in aws_route_table.private : r.id] }
output "security_groups" { value = { for n, s in aws_security_group.this : n => s.id } }
output "nat_count" { value = length(aws_nat_gateway.this) }
output "nat_gateway_ids" {
  description = "Explicit NAT Gateway IDs when NAT is provisioned."
  value       = { for z, g in aws_nat_gateway.this : z => g.id }
}
output "elastic_ip_allocation_ids" {
  description = "Explicit Elastic IP allocation IDs when NAT is provisioned."
  value       = { for z, e in aws_eip.nat : z => e.id }
}

output "public_subnet_cidrs" { value = { for z, s in aws_subnet.public : z => s.cidr_block } }

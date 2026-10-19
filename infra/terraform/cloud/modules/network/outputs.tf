output "vpc_id" { value = aws_vpc.this.id }
output "public_subnets" { value = { for z, s in aws_subnet.public : z => s.id } }
output "private_subnets" { value = { for z, s in aws_subnet.private : z => s.id } }
output "private_route_table_ids" { value = [for r in aws_route_table.private : r.id] }
output "security_groups" { value = { for n, s in aws_security_group.this : n => s.id } }
output "nat_count" { value = length(aws_nat_gateway.this) }

output "public_subnet_cidrs" { value = { for z, s in aws_subnet.public : z => s.cidr_block } }

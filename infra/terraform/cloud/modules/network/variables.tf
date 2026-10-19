variable "name" { type = string }
variable "cidr" { type = string }
variable "zones" { type = list(string) }
variable "egress_mode" { type = string }
variable "nat_per_az" { type = bool }
variable "client_cidrs" { type = set(string) }
variable "admin_ssh_cidrs" { type = set(string) }

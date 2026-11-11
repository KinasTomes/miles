variable "enabled" { type = bool }
variable "name" { type = string }
variable "account_id" { type = string }
variable "region" { type = string }
variable "ami_id" { type = string }
variable "instance_type" { type = string }
variable "architecture" { type = string }
variable "subnet_ids" { type = list(string) }
variable "security_group_id" { type = string }
variable "instance_profile" { type = string }
variable "service_linked_role_arn" { type = string }
variable "kms_key_arn" { type = string }
variable "target_group_arn" { type = string }
variable "data_bucket_name" { type = string }
variable "release_sha256" { type = string }
variable "release_version_id" { type = string }
variable "runtime_secret_arn" { type = string }
variable "health_check_type" {
  type        = string
  default     = "EC2"
  description = "Health check type for ASG. Defaults to 'EC2' to separate traffic-routing readiness (handled by ALB target group /readyz) from instance liveness, avoiding unnecessary instance churn on transient readiness blips."
  validation {
    condition     = contains(["EC2", "ELB"], var.health_check_type)
    error_message = "health_check_type must be either 'EC2' or 'ELB'."
  }
}
variable "owner_ssh_public_key" {
  type    = string
  default = null
  validation {
    condition     = var.owner_ssh_public_key == null || can(regex("^ssh-ed25519 [A-Za-z0-9+/=]+( [^\\r\\n]+)?$", var.owner_ssh_public_key))
    error_message = "Owner SSH uses one reviewed ed25519 public key, never a private key."
  }
}
variable "deep_observation" {
  type        = bool
  default     = false
  description = "Enable opt-in bounded DynamoDB scans for stuck completing uploads and journal export lag in gateway startup."
}
variable "enable_test_fault_injection" {
  type        = bool
  default     = false
  description = "Enable node-scoped fault injection only for an isolated acceptance fleet."
}

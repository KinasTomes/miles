terraform {
  required_version = "= 1.15.2"
  required_providers {
    aws = { source = "hashicorp/aws", version = "= 6.21.0" }
  }
}
provider "aws" {
  region              = var.region
  allowed_account_ids = [var.account_id]
  default_tags { tags = { Project = "Miles", Installation = var.installation_name, Environment = var.environment, Owner = var.owner, ManagedBy = "Terraform" } }
}
variable "account_id" { type = string }
variable "region" { type = string }
variable "installation_name" { type = string }
variable "environment" { type = string }
variable "owner" { type = string }
variable "operator_user_arn" { type = string }
variable "deploy_role_arn" { type = string }
variable "boundary_arn" { type = string }
variable "boundary_document" { type = string }
variable "user_document" { type = string }
variable "trust_document" { type = string }
variable "deploy_documents" { type = map(string) }
locals {
  user_resource     = split(":user/", var.operator_user_arn)[1]
  role_resource     = split(":role/", var.deploy_role_arn)[1]
  boundary_resource = split(":policy/", var.boundary_arn)[1]
}
resource "aws_iam_user" "operator" {
  name          = basename(local.user_resource)
  path          = "/${trimsuffix(local.user_resource, basename(local.user_resource))}"
  force_destroy = false
}
resource "aws_iam_policy" "boundary" {
  name   = basename(local.boundary_resource)
  path   = "/${trimsuffix(local.boundary_resource, basename(local.boundary_resource))}"
  policy = var.boundary_document
  lifecycle { prevent_destroy = true }
}
resource "aws_iam_role" "deploy" {
  name                 = basename(local.role_resource)
  path                 = "/${trimsuffix(local.role_resource, basename(local.role_resource))}"
  assume_role_policy   = var.trust_document
  max_session_duration = 3600
  depends_on           = [aws_iam_user.operator, aws_iam_policy.boundary]
}
resource "aws_iam_user_policy" "operator" {
  name   = "${var.installation_name}-operator"
  user   = aws_iam_user.operator.name
  policy = var.user_document
}
resource "aws_iam_policy" "deploy" {
  for_each = var.deploy_documents
  name     = "${var.installation_name}-deploy-${each.key}"
  path     = "/miles/bootstrap/"
  policy   = each.value
}
resource "aws_iam_role_policy_attachment" "deploy" {
  for_each   = aws_iam_policy.deploy
  role       = aws_iam_role.deploy.name
  policy_arn = each.value.arn
}
resource "aws_iam_role_policy_attachment" "signin" {
  role       = aws_iam_role.deploy.name
  policy_arn = "arn:aws:iam::aws:policy/SignInLocalDevelopmentAccess"
}
resource "aws_iam_user_policy_attachment" "signin" {
  user       = aws_iam_user.operator.name
  policy_arn = "arn:aws:iam::aws:policy/SignInLocalDevelopmentAccess"
}
output "operator_user_arn" { value = aws_iam_user.operator.arn }
output "deploy_role_arn" { value = aws_iam_role.deploy.arn }

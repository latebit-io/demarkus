---
layout: default
title: Blog
permalink: /blog/
---

# Blog

Notes, ideas, and design thinking from the demarkus project.

{% if site.posts.size == 0 %}
_No posts yet._
{% else %}
<ul class="post-list">
{% for post in site.posts %}
  <li>
    <a href="{{ post.url | relative_url }}">{{ post.title }}</a>
    <span class="post-date">{{ post.date | date: "%Y-%m-%d" }}</span>
  </li>
{% endfor %}
</ul>
{% endif %}

// CloudBox для Android и iOS: файлы, загрузка, автозагрузка фото.
import 'package:flutter/material.dart';

import 'api.dart';
import 'autoupload.dart';
import 'screens/files.dart';
import 'screens/login.dart';
import 'storage.dart';

void main() {
  WidgetsFlutterBinding.ensureInitialized();
  runApp(const CloudBoxApp());
}

class CloudBoxApp extends StatelessWidget {
  const CloudBoxApp({super.key, this.home});
  final Widget? home; // для тестов: показать конкретный экран

  @override
  Widget build(BuildContext context) {
    const seed = Color(0xFF2F6FED);
    return MaterialApp(
      title: 'CloudBox',
      debugShowCheckedModeBanner: false,
      theme: ThemeData(colorSchemeSeed: seed, useMaterial3: true),
      darkTheme: ThemeData(colorSchemeSeed: seed, brightness: Brightness.dark, useMaterial3: true),
      home: home ?? const StartScreen(),
    );
  }
}

/// StartScreen — вход по сохранённым данным или экран входа.
class StartScreen extends StatefulWidget {
  const StartScreen({super.key});
  @override
  State<StartScreen> createState() => _StartScreenState();
}

class _StartScreenState extends State<StartScreen> {
  @override
  void initState() {
    super.initState();
    _start();
  }

  Future<void> _start() async {
    final acc = await Storage.load();
    if (!mounted) return;
    if (acc == null) {
      Navigator.of(context).pushReplacement(MaterialPageRoute(builder: (_) => const LoginScreen()));
      return;
    }
    final api = CloudBoxApi(acc.server, acc.user, acc.password);
    Navigator.of(context).pushReplacement(MaterialPageRoute(builder: (_) => FilesScreen(api: api)));
    // автозагрузка при открытии приложения (если включена) — в фоне, не мешая работе
    AutoUpload.run(api).ignore();
  }

  @override
  Widget build(BuildContext context) => const Scaffold(body: Center(child: CircularProgressIndicator()));
}
